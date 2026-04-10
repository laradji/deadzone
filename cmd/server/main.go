package main

import (
	"context"
	"log"

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

func handleSearchDocs(ctx context.Context, req *mcp.CallToolRequest, input SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
	// TODO: query FTS5 via internal/search
	return nil, SearchDocsOutput{Snippets: []Snippet{}}, nil
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "deadzone", Version: "v0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_docs",
		Description: "Search documentation snippets for a library. Use lib_id in /org/project format to filter by library.",
	}, handleSearchDocs)

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
