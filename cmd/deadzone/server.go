package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/laradji/deadzone/internal/logs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Library IDs follow the format /org/project (e.g. /hashicorp/terraform)
type SearchDocsInput struct {
	Query  string `json:"query" jsonschema:"the search query"`
	LibID  string `json:"lib_id,omitempty" jsonschema:"library ID in /org/project format (optional)"`
	Tokens int    `json:"tokens,omitempty" jsonschema:"max tokens to return, min 1000, default 5000 (optional)"`
}

type Snippet struct {
	LibID   string `json:"lib_id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type SearchDocsOutput struct {
	Snippets []Snippet `json:"snippets"`
}

const (
	defaultTokens = 5000
	minTokens     = 1000
	charsPerToken = 4
	searchK       = 10
)

func makeSearchHandler(d *db.DB, e embed.Embedder, verbose bool) func(context.Context, *mcp.CallToolRequest, SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
		start := time.Now()

		queryVec, err := e.EmbedQuery(input.Query)
		if err != nil {
			slog.Error("search_docs failed", searchAttrs(input, verbose, "stage", "embed", "err", err.Error())...)
			return nil, SearchDocsOutput{}, fmt.Errorf("embed query: %w", err)
		}
		docs, err := db.SearchByEmbedding(d, queryVec, input.LibID, searchK)
		if err != nil {
			slog.Error("search_docs failed", searchAttrs(input, verbose, "stage", "search", "err", err.Error())...)
			return nil, SearchDocsOutput{}, err
		}

		tokens := input.Tokens
		if tokens < minTokens {
			tokens = defaultTokens
		}
		budget := tokens * charsPerToken

		var snippets []Snippet
		remaining := budget
		for _, doc := range docs {
			if remaining <= 0 {
				break
			}
			content := doc.Content
			if len(content) > remaining {
				content = content[:remaining]
			}
			snippets = append(snippets, Snippet{
				LibID:   doc.LibID,
				Title:   doc.Title,
				Content: content,
			})
			remaining -= len(content)
		}

		if snippets == nil {
			snippets = []Snippet{}
		}

		slog.Info("search_docs", searchAttrs(input, verbose,
			"tokens", tokens,
			"results", len(snippets),
			"latency_ms", time.Since(start).Milliseconds(),
		)...)

		return nil, SearchDocsOutput{Snippets: snippets}, nil
	}
}

// searchAttrs builds the slog key/value list shared between the success
// and error log lines for search_docs. The verbose flag adds the raw
// query text — gated because queries may contain user data routed
// through the LLM and we don't want it in default logs.
func searchAttrs(input SearchDocsInput, verbose bool, extra ...any) []any {
	attrs := make([]any, 0, 4+len(extra))
	attrs = append(attrs, "lib_id", input.LibID)
	attrs = append(attrs, extra...)
	if verbose {
		attrs = append(attrs, "query", input.Query)
	}
	return attrs
}

// SearchLibrariesInput is the JSON shape accepted by the search_libraries
// MCP tool. Name is free-text — the handler embeds it with the same hugot
// pipeline used at index time and matches against libs.embedding via
// vector_distance_cos. An empty Name is the cheap "what's even in here"
// path that returns the top-K libs by doc_count without an embedder
// call.
type SearchLibrariesInput struct {
	Name  string `json:"name,omitempty" jsonschema:"free-text library name to resolve; empty returns top libs by doc count"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 10, max 50"`
}

// LibraryHit is one ranked candidate returned by search_libraries.
// MatchScore is 1 - cosine_distance(query, lib_embedding) so higher is
// closer; the empty-name path returns 1.0 for every row (no query was
// embedded). LLM clients can use the score to decide whether to commit
// to a single result or surface multiple candidates to the user.
type LibraryHit struct {
	LibID      string  `json:"lib_id"`
	DocCount   int     `json:"doc_count"`
	MatchScore float32 `json:"match_score"`
}

// SearchLibrariesOutput is the wire envelope. The Libraries slice is
// always non-nil (no matches → empty, never null) so MCP clients can
// iterate without a nil-guard.
type SearchLibrariesOutput struct {
	Libraries []LibraryHit `json:"libraries"`
}

const (
	defaultLibLimit = 10
	maxLibLimit     = 50
)

// makeSearchLibrariesHandler closes over the DB and Embedder so the
// MCP tool registration can stay a one-liner. The empty-name branch
// goes straight to TopLibsByDocCount and skips e.Embed entirely; the
// non-empty branch is symmetric with search_docs (embed once, query
// once). The 1 - dist score conversion happens here so LibInfo's raw
// distance never escapes the package.
func makeSearchLibrariesHandler(d *db.DB, e embed.Embedder, verbose bool) func(context.Context, *mcp.CallToolRequest, SearchLibrariesInput) (*mcp.CallToolResult, SearchLibrariesOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input SearchLibrariesInput) (*mcp.CallToolResult, SearchLibrariesOutput, error) {
		start := time.Now()

		limit := input.Limit
		if limit <= 0 {
			limit = defaultLibLimit
		}
		if limit > maxLibLimit {
			limit = maxLibLimit
		}

		name := strings.TrimSpace(input.Name)

		var (
			libs []db.LibInfo
			err  error
		)
		if name == "" {
			libs, err = db.TopLibsByDocCount(d, limit)
			if err != nil {
				slog.Error("search_libraries failed", libAttrs(input, name, limit, verbose, "stage", "top_libs", "err", err.Error())...)
				return nil, SearchLibrariesOutput{}, err
			}
		} else {
			queryVec, embedErr := e.EmbedQuery(name)
			if embedErr != nil {
				slog.Error("search_libraries failed", libAttrs(input, name, limit, verbose, "stage", "embed", "err", embedErr.Error())...)
				return nil, SearchLibrariesOutput{}, fmt.Errorf("embed query: %w", embedErr)
			}
			libs, err = db.SearchLibsByEmbedding(d, queryVec, limit)
			if err != nil {
				slog.Error("search_libraries failed", libAttrs(input, name, limit, verbose, "stage", "search", "err", err.Error())...)
				return nil, SearchLibrariesOutput{}, err
			}
		}

		// make([]LibraryHit, 0, len(libs)) is the load-bearing call
		// here: it guarantees a non-nil empty slice on the no-matches
		// path, which is one of the issue's acceptance criteria.
		hits := make([]LibraryHit, 0, len(libs))
		for _, lib := range libs {
			hits = append(hits, LibraryHit{
				LibID:      lib.LibID,
				DocCount:   lib.DocCount,
				MatchScore: 1.0 - lib.Distance,
			})
		}

		slog.Info("search_libraries", libAttrs(input, name, limit, verbose,
			"results", len(hits),
			"latency_ms", time.Since(start).Milliseconds(),
		)...)

		return nil, SearchLibrariesOutput{Libraries: hits}, nil
	}
}

// libAttrs is the search_libraries equivalent of searchAttrs: a single
// place to assemble the slog key/value list shared by success and
// error paths. The raw name is gated behind -verbose for the same
// reason search_docs gates query text — names may carry user data
// routed through the LLM.
func libAttrs(input SearchLibrariesInput, name string, limit int, verbose bool, extra ...any) []any {
	attrs := make([]any, 0, 6+len(extra))
	attrs = append(attrs, "name_len", len(name), "limit", limit)
	attrs = append(attrs, extra...)
	if verbose {
		attrs = append(attrs, "name", input.Name)
	}
	return attrs
}

// runServer is the `deadzone server` entry point. The body is the
// former cmd/server/main.go run(), with `flag.*` replaced by a per-sub
// flag.FlagSet so the top-level dispatch can own os.Args without
// colliding with the sibling subcommands' flag definitions.
func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	dbPath := fs.String("db", "deadzone.db", "path to turso database file")
	embedderKind := fs.String("embedder", embed.KindHugot, "embedder to use (valid: hugot)")
	verbose := fs.Bool("verbose", false, "include the raw query text in per-call logs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Wire slog before any other work so subsequent error paths emit
	// structured JSON to stderr — never stdout, which is the MCP
	// JSON-RPC channel.
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	// The server is a read-only consumer of the consolidated DB; it
	// must NOT auto-create a fresh empty file (that would silently
	// hide a missed `consolidate` step and serve zero results to
	// every query). Stat first; if missing, point the operator at
	// the consolidate subcommand before any other work happens.
	if _, err := os.Stat(*dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s not found. Run `deadzone consolidate -db %s -artifacts ./artifacts` first to merge per-lib artifacts into the main database", *dbPath, *dbPath)
	} else if err != nil {
		return fmt.Errorf("stat db %s: %w", *dbPath, err)
	}

	// db.Open validates the embedder's reported meta against whatever
	// the database was created with; a mismatch fails fast and tells
	// the user to rebuild against a fresh file.
	e, err := embed.New(*embedderKind)
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	defer func() {
		if err := e.Close(); err != nil {
			slog.Warn("embedder close", "err", err.Error())
		}
	}()

	d, err := db.Open(*dbPath, db.Meta{
		EmbedderKind: e.Kind(),
		EmbeddingDim: e.Dim(),
		ModelVersion: e.ModelVersion(),
	})
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	// Read the doc count once at startup so operators see corpus size
	// in the banner without having to run a separate query. Cheap on
	// the corpora deadzone targets (a few hundred to a few thousand
	// rows); revisit if this ever becomes hot.
	var docCount int
	if err := d.QueryRow(`SELECT count(*) FROM docs`).Scan(&docCount); err != nil {
		return fmt.Errorf("count docs: %w", err)
	}

	slog.Info("server.start",
		"version", version,
		"commit", commit,
		"build_date", date,
		"db_path", *dbPath,
		"embedder_kind", e.Kind(),
		"embedding_dim", e.Dim(),
		"model_version", e.ModelVersion(),
		"doc_count", docCount,
	)

	s := mcp.NewServer(&mcp.Implementation{Name: "deadzone", Version: version}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_docs",
		Description: "Search documentation snippets for a library. Use lib_id in /org/project format to filter by library.",
	}, makeSearchHandler(d, e, *verbose))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_libraries",
		Description: "Resolve a free-text library name into a ranked list of canonical lib_id candidates that can be passed to search_docs. Pass an empty name to list the most-indexed libraries by doc_count.",
	}, makeSearchLibrariesHandler(d, e, *verbose))

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp run: %w", err)
	}
	return nil
}
