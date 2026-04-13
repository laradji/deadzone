package packs_test

import (
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/packs"
)

func TestSlug(t *testing.T) {
	cases := []struct {
		libID string
		want  string
	}{
		{"/modelcontextprotocol/go-sdk", "modelcontextprotocol_go-sdk"},
		{"/facebook/react/v18", "facebook_react_v18"},
		{"/org/project", "org_project"},
		{"/org/project-with.dots", "org_project-with.dots"},
	}
	for _, c := range cases {
		if got := packs.Slug(c.libID); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.libID, got, c.want)
		}
	}
}

func TestArtifactDir(t *testing.T) {
	if got := packs.ArtifactDir("./artifacts", "/facebook/react/v18"); got != filepath.Join("artifacts", "facebook_react_v18") {
		t.Errorf("ArtifactDir = %q", got)
	}
}

func TestArtifactDBPath(t *testing.T) {
	want := filepath.Join("artifacts", "modelcontextprotocol_go-sdk", "artifact.db")
	if got := packs.ArtifactDBPath("./artifacts", "/modelcontextprotocol/go-sdk"); got != want {
		t.Errorf("ArtifactDBPath = %q, want %q", got, want)
	}
}

func TestStatePath(t *testing.T) {
	want := filepath.Join("artifacts", "facebook_react_v18", "state.yaml")
	if got := packs.StatePath("./artifacts", "/facebook/react/v18"); got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
}
