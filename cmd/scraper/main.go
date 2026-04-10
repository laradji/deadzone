package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/laradji/deadzone/internal/scraper"
)

const goSDKLibID = "/modelcontextprotocol/go-sdk"

const rawBase = "https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/"

var goSDKURLs = []string{
	rawBase + "README.md",
	rawBase + "docs/README.md",
	rawBase + "docs/quick_start.md",
	rawBase + "docs/server.md",
	rawBase + "docs/client.md",
	rawBase + "docs/protocol.md",
	rawBase + "docs/mcpgodebug.md",
	rawBase + "docs/troubleshooting.md",
	rawBase + "docs/rough_edges.md",
}

func main() {
	dbPath := flag.String("db", "deadzone.db", "path to turso database file")
	flag.Parse()

	d, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer d.Close()

	e := embed.NewStub()

	src := scraper.Source{
		LibID: goSDKLibID,
		URLs:  goSDKURLs,
	}

	log.Printf("scraping %d URLs for %s", len(src.URLs), src.LibID)

	docs, err := scraper.Fetch(context.Background(), http.DefaultClient, src)
	if err != nil {
		log.Fatalf("fetch: %v", err)
	}

	for _, doc := range docs {
		vec := e.Embed(doc.Title + "\n" + doc.Content)
		if err := db.Insert(d, doc, vec); err != nil {
			log.Fatalf("insert %q: %v", doc.Title, err)
		}
	}

	log.Printf("indexed %d docs (dim=%d) for %s into %s", len(docs), embed.Dim, src.LibID, *dbPath)
}
