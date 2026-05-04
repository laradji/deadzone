package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/ort"
)

// TestOrtMetaJSONShape locks the JSON contract that release.yml's
// docker job and the docker-build justfile recipe parse with jq. A
// breaking change here breaks the OCI image build before it can fail
// loud at `docker buildx build` time, so the test is the cheapest place
// to catch it.
func TestOrtMetaJSONShape(t *testing.T) {
	var buf bytes.Buffer
	if err := runOrtMeta(&buf); err != nil {
		t.Fatalf("runOrtMeta: %v (output: %s)", err, buf.String())
	}

	var out ortMetaOutput
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON: %v (output: %s)", err, buf.String())
	}

	if out.Version != ort.Version {
		t.Errorf("version = %q, want %q", out.Version, ort.Version)
	}

	// Both linux arches must be present and only linux entries — the
	// OCI image is Linux-only, and emitting darwin/windows would
	// tempt callers to bake them.
	have := map[string]ortMetaPlatform{}
	for _, p := range out.Platforms {
		key := p.GOOS + "/" + p.GOARCH
		have[key] = p
		if p.GOOS != "linux" {
			t.Errorf("non-linux platform %q in output (must be filtered out)", key)
		}
		if !strings.Contains(p.URL, "/v"+ort.Version+"/") {
			t.Errorf("%s: url missing /v%s/ segment: %q", key, ort.Version, p.URL)
		}
		if len(p.SHA256) != 64 {
			t.Errorf("%s: sha256 length = %d, want 64", key, len(p.SHA256))
		}
		if p.LibName == "" {
			t.Errorf("%s: lib_name empty", key)
		}
	}
	for _, want := range []string{"linux/amd64", "linux/arm64"} {
		if _, ok := have[want]; !ok {
			t.Errorf("output missing platform %q (have: %v)", want, have)
		}
	}
}
