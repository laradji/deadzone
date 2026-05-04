package godoc_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/scraper/godoc"
)

var updateGolden = flag.Bool("update-golden", false, "regenerate testdata/sample.golden from current ParseGodoc output (also enabled by UPDATE_GOLDEN=1)")

func TestParseGodoc_GoldenFixture(t *testing.T) {
	docs, err := godoc.ParseGodoc("/test/sample", filepath.Join("testdata", "sample"))
	if err != nil {
		t.Fatalf("ParseGodoc: %v", err)
	}

	got := serializeChunks(docs)
	goldenPath := filepath.Join("testdata", "sample.golden")

	if *updateGolden || os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d chunks)", goldenPath, len(docs))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to seed): %v", err)
	}
	if got != string(want) {
		t.Errorf("ParseGodoc output diverges from %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, string(want))
	}

	// Cross-checks that don't depend on the golden file shape — these
	// catch regressions in the chunk-shape contract from §3 of the
	// research doc even if someone updates the golden carelessly.
	titles := make(map[string]bool, len(docs))
	for _, d := range docs {
		if d.LibID != "/test/sample" {
			t.Errorf("Doc %q: LibID = %q, want /test/sample", d.Title, d.LibID)
		}
		if strings.TrimSpace(d.Content) == "" {
			t.Errorf("Doc %q has empty Content", d.Title)
		}
		titles[d.Title] = true
	}
	mustHave := []string{
		"sample package",
		"sample.Config",       // type, with methods + ctors rolled in
		"sample.Option",       // type with one constructor
		"sample.Hello",        // top-level func (returns builtin string)
		"sample.Greeting",     // first-named single const
		"sample.MaxItems ...", // multi-name const group
		"sample.Defaults",     // single-named var
	}
	for _, want := range mustHave {
		if !titles[want] {
			t.Errorf("expected chunk title %q not found in output", want)
		}
	}
	mustNotHave := []string{
		"sample.unexportedHelper",
		"sample.TestSomething",
		// Constructors are filed under their returned type by go/doc
		// (Type.Funcs), so they are rolled into the type chunk and
		// must NOT appear standalone.
		"sample.NewConfig",
		"sample.WithVerbose",
		// Methods are similarly rolled into the type's Content.
		"sample.Apply",
	}
	for _, ban := range mustNotHave {
		if titles[ban] {
			t.Errorf("chunk %q must not appear standalone", ban)
		}
	}

	// The rolled-in Constructors / Methods must be present *inside*
	// the type chunk's Content. Find the Config chunk and assert.
	var configContent string
	for _, d := range docs {
		if d.Title == "sample.Config" {
			configContent = d.Content
			break
		}
	}
	if !strings.Contains(configContent, "NewConfig") {
		t.Errorf("sample.Config Content missing constructor NewConfig:\n%s", configContent)
	}
	if !strings.Contains(configContent, "Apply") {
		t.Errorf("sample.Config Content missing method Apply:\n%s", configContent)
	}
}

// serializeChunks renders []db.Doc as deterministic NDJSON, one Doc
// per line. JSON handles content-newline escaping natively, which
// keeps the golden file diff-friendly without custom delimiter quirks.
// Only Title and Content are serialized — LibID is checked separately
// (it's just stamped) and Version is always empty here.
func serializeChunks(docs []db.Doc) string {
	var buf strings.Builder
	for _, d := range docs {
		row := struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}{Title: d.Title, Content: d.Content}
		b, err := json.Marshal(row)
		if err != nil {
			// json.Marshal of two strings cannot fail in practice;
			// surface as a literal error line so the golden diff
			// makes the bug obvious instead of silently dropping it.
			buf.WriteString("MARSHAL_ERROR: " + err.Error() + "\n")
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.String()
}
