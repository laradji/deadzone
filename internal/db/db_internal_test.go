package db

import (
	"database/sql"
	"encoding/json"
	"math"
	"testing"

	_ "turso.tech/database/tursogo"
)

// TestFormatVector_Roundtrip verifies that formatVector produces a string
// accepted by turso's vector() constructor when bound through a prepared
// statement parameter (i.e. `vector(?)` with the JSON string as a TEXT arg),
// and that vector_extract reads back the same floats within float32 precision.
//
// This is the "Decision B" probe from the implementation plan: if bound
// parameters don't work, we fall back to inline fmt.Sprintf. The upstream
// test suite only exercises inline literals, so this regression check is
// load-bearing.
func TestFormatVector_Roundtrip(t *testing.T) {
	d, err := sql.Open("turso", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	// tursogo is BETA; serialize to match what internal/db will do.
	d.SetMaxOpenConns(1)

	if _, err := d.Exec(`CREATE TABLE v (id INTEGER PRIMARY KEY, embedding F32_BLOB(4))`); err != nil {
		t.Fatalf("create: %v", err)
	}

	orig := []float32{0.1, 0.2, 0.3, 0.4}
	blob := formatVector(orig)

	if _, err := d.Exec(`INSERT INTO v (id, embedding) VALUES (1, vector(?))`, blob); err != nil {
		t.Fatalf("insert via vector(?): %v", err)
	}

	var extracted string
	if err := d.QueryRow(`SELECT vector_extract(embedding) FROM v WHERE id = 1`).Scan(&extracted); err != nil {
		t.Fatalf("extract: %v", err)
	}

	var got []float32
	if err := json.Unmarshal([]byte(extracted), &got); err != nil {
		t.Fatalf("parse vector_extract output %q: %v", extracted, err)
	}
	if len(got) != len(orig) {
		t.Fatalf("len mismatch: got %d, want %d", len(got), len(orig))
	}
	for i := range orig {
		if math.Abs(float64(got[i]-orig[i])) > 1e-6 {
			t.Errorf("component %d: got %v, want %v (Δ=%v)", i, got[i], orig[i], got[i]-orig[i])
		}
	}
}
