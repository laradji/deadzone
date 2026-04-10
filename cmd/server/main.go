package main

import (
	"context"
	"flag"
	"log"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Library IDs follow the format /org/project (e.g. /hashicorp/terraform)
type SearchDocsInput struct {
	Query  string `json:"query" jsonschema:"the search query"`
	LibID  string `json:"lib_id,omitempty" jsonschema:"library ID in /org/project format (optional)"`
	Topic  string `json:"topic,omitempty" jsonschema:"topic or section to focus on (optional)"`
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

func makeSearchHandler(d *db.DB, e embed.Embedder) func(context.Context, *mcp.CallToolRequest, SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
		queryVec := e.Embed(input.Query)
		docs, err := db.SearchByEmbedding(d, queryVec, input.LibID, searchK)
		if err != nil {
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

		return nil, SearchDocsOutput{Snippets: snippets}, nil
	}
}

func main() {
	dbPath := flag.String("db", "deadzone.db", "path to turso database file")
	embedderKind := flag.String("embedder", embed.KindHugot, "embedder to use (valid: hugot)")
	flag.Parse()

	// db.Open validates the embedder's reported meta against whatever
	// the database was created with; a mismatch fails fast and tells
	// the user to rebuild against a fresh file.
	e, err := embed.New(*embedderKind)
	if err != nil {
		log.Fatalf("embedder: %v", err)
	}
	if c, ok := e.(interface{ Close() error }); ok {
		defer func() {
			if err := c.Close(); err != nil {
				log.Printf("embedder close: %v", err)
			}
		}()
	}

	d, err := db.Open(*dbPath, db.Meta{
		EmbedderKind: e.Kind(),
		EmbeddingDim: e.Dim(),
		ModelVersion: e.ModelVersion(),
	})
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer d.Close()

	s := mcp.NewServer(&mcp.Implementation{Name: "deadzone", Version: "v0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_docs",
		Description: "Search documentation snippets for a library. Use lib_id in /org/project format to filter by library.",
	}, makeSearchHandler(d, e))

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
