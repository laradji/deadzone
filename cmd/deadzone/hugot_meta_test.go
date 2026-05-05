package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/embed"
)

// TestHugotMetaJSONShape locks the JSON contract that scrape-pack.yml's
// docker job parses with jq. A breaking change here breaks the OCI
// image build before it can fail loud at `docker buildx build` time, so
// the test is the cheapest place to catch it.
//
// Mirrors TestOrtMetaJSONShape in shape and intent — same rationale,
// different constants table.
func TestHugotMetaJSONShape(t *testing.T) {
	var buf bytes.Buffer
	if err := runHugotMeta(&buf); err != nil {
		t.Fatalf("runHugotMeta: %v (output: %s)", err, buf.String())
	}

	var out hugotMetaOutput
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON: %v (output: %s)", err, buf.String())
	}

	if out.ModelID != embed.DefaultHugotModel {
		t.Errorf("model_id = %q, want %q", out.ModelID, embed.DefaultHugotModel)
	}

	// Revision must be a 40-char lowercase hex commit SHA. HF's
	// content-addressable URL only stays bytes-stable when the path
	// segment is a real commit; pointing at "main" or a tag would
	// silently drift on every upstream push.
	if got := embed.ModelRevision; len(got) != 40 {
		t.Errorf("ModelRevision length = %d, want 40 (HF commit SHA)", len(got))
	}
	if out.Revision != embed.ModelRevision {
		t.Errorf("revision = %q, want %q", out.Revision, embed.ModelRevision)
	}

	// dest_dirname must match the "/" → "_" substitution rule hugot's
	// downloader applies (hugot.go:127). The image staging step lays
	// files out under dist/linux_<arch>/models/<dest_dirname>/, so a
	// drift here breaks the COPY at Dockerfile build time.
	wantDir := strings.ReplaceAll(embed.DefaultHugotModel, "/", "_")
	if out.DestDirname != wantDir {
		t.Errorf("dest_dirname = %q, want %q", out.DestDirname, wantDir)
	}

	// 6 files is the empirically-derived shape (see model_pin.go's
	// pinnedModelFiles comment). A future hugot bump that needs a 7th
	// file will fail this test loud — at which point the contributor
	// adds the file to model_pin.go AND bumps this expected count, in
	// one PR, with the cache-invalidation reasoning in the description.
	const wantFiles = 6
	if len(out.Files) != wantFiles {
		t.Errorf("len(files) = %d, want %d", len(out.Files), wantFiles)
	}

	// Every file entry is fully populated. Each field is load-bearing
	// for the docker job's `curl -fL -o ... && sha256sum -c -` step;
	// a blank URL makes curl emit a confusing error on a "://" path,
	// a blank dest_name overwrites whichever sibling file landed
	// alphabetically first.
	for i, f := range out.Files {
		if !strings.HasPrefix(f.URL, "https://huggingface.co/") {
			t.Errorf("files[%d].url = %q, want huggingface.co prefix", i, f.URL)
		}
		if !strings.Contains(f.URL, "/resolve/"+embed.ModelRevision+"/") {
			t.Errorf("files[%d].url = %q, want /resolve/<ModelRevision>/ segment", i, f.URL)
		}
		if len(f.SHA256) != 64 {
			t.Errorf("files[%d].sha256 length = %d, want 64", i, len(f.SHA256))
		}
		if f.DestName == "" {
			t.Errorf("files[%d].dest_name empty", i)
		}
		if strings.ContainsAny(f.DestName, "/\\") {
			// dest_name is a basename; keep nesting out of it. The
			// staging step writes to <dest_dirname>/<dest_name>, and a
			// "/" inside dest_name would let the staging step write
			// outside the model directory in the worst case.
			t.Errorf("files[%d].dest_name = %q, want basename without separators", i, f.DestName)
		}
	}

	// The ONNX file is the one entry whose source path differs from
	// its dest name (HF's onnx/ subdir → flattened in the cache). If
	// that flattening drifts, hugot.NewPipeline silently picks the
	// wrong .onnx file (or none). Anchor it explicitly here.
	var sawOnnx bool
	for _, f := range out.Files {
		if f.DestName == "model_quantized.onnx" {
			sawOnnx = true
			if !strings.Contains(f.URL, "/onnx/model_quantized.onnx") {
				t.Errorf("ONNX file URL %q missing expected /onnx/ path segment", f.URL)
			}
		}
	}
	if !sawOnnx {
		t.Error("output missing model_quantized.onnx entry")
	}
}
