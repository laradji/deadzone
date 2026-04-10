package main

import (
	"context"
	"database/sql"
	"flag"
	"log"

	"github.com/laradji/deadzone/internal/db"
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
)

func makeSearchHandler(d *sql.DB) func(context.Context, *mcp.CallToolRequest, SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
		docs, err := db.Search(d, input.Query, input.LibID)
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
	dbPath := flag.String("db", "deadzone.db", "path to libSQL database file")
	flag.Parse()

	d, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer d.Close()

	s := mcp.NewServer(&mcp.Implementation{Name: "deadzone", Version: "v0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_docs",
		Description: "Search documentation snippets for a library. Use lib_id in /org/project format to filter by library.",
	}, makeSearchHandler(d))

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
