package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takodotid/tack/internal/version"
)

// bareDocPaths returns each docs/ path in s that is not part of a link of
// version.DocLink.
func bareDocPaths(s string) []string {
	prefix := version.DocLink("")
	var out []string
	for i := 0; ; {
		j := strings.Index(s[i:], "docs/")
		if j < 0 {
			return out
		}
		i += j
		if !strings.HasSuffix(s[:i], prefix) {
			end := strings.IndexAny(s[i:], " \n)")
			if end < 0 {
				end = len(s) - i
			}
			out = append(out, s[i:i+end])
		}
		i += len("docs/")
	}
}

// The plans and the skill document of the golden files, and the man pages,
// name each doc by its link, never by its path in the repository (#51).
func TestOutputsLinkDocs(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil || len(files) == 0 {
		t.Fatalf("golden files: %v, %v", files, err)
	}
	dir := t.TempDir()
	if err := WriteMan(dir, fixedNow); err != nil {
		t.Fatal(err)
	}
	pages, err := filepath.Glob(filepath.Join(dir, "*.1"))
	if err != nil || len(pages) == 0 {
		t.Fatalf("man pages: %v, %v", pages, err)
	}
	links := 0
	for _, f := range append(files, pages...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bare := bareDocPaths(string(b)); len(bare) > 0 {
			t.Errorf("%s names docs by their path: %q", filepath.Base(f), bare)
		}
		links += strings.Count(string(b), version.DocLink("docs/"))
	}
	if links == 0 {
		t.Fatal("no output links a doc")
	}
}

// The help of a release build links the docs at its tag.
func TestHelpLinksReleaseTag(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v1.3.0"
	help := map[string]string{}
	for _, c := range NewRoot(nil).Commands() {
		help[c.Name()] = c.Long
	}
	for cmd, want := range map[string]string{
		"run":     "https://github.com/takodotid/tack/blob/v1.3.0/docs/runbooks/site-loss.md",
		"abandon": "https://github.com/takodotid/tack/blob/v1.3.0/docs/runbooks/abandon-run.md",
	} {
		if !strings.Contains(help[cmd], want) {
			t.Errorf("tack %s --help lacks %s:\n%s", cmd, want, help[cmd])
		}
	}
}
