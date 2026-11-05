package version

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// root is the repository's root, from this package's directory.
const root = "../.."

// literal is a string literal of the module's code.
type literal struct {
	pos   token.Position
	value string
	// link is set when the literal is the argument of a DocLink call.
	link bool
}

// docLinkCall reports whether call calls DocLink: version.DocLink from
// another package, or DocLink in this one.
func docLinkCall(call *ast.CallExpr, pkg string) bool {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == "version" && f.Sel.Name == "DocLink"
	case *ast.Ident:
		return pkg == "version" && f.Name == "DocLink"
	}
	return false
}

// literals returns every string literal in the module's Go code but its
// tests, and the position of each DocLink call whose argument is not a
// string literal. Test data and other modules are left out.
func literals(t *testing.T) (lits []literal, bad []token.Position) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "dist", "bin", ".e2e":
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		args := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if !docLinkCall(n, f.Name.Name) {
					return true
				}
				if len(n.Args) == 1 {
					if l, ok := n.Args[0].(*ast.BasicLit); ok && l.Kind == token.STRING {
						args[l] = true
						return true
					}
				}
				bad = append(bad, fset.Position(n.Pos()))
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				v, err := strconv.Unquote(n.Value)
				if err != nil {
					t.Errorf("%s: %v", fset.Position(n.Pos()), err)
					return true
				}
				lits = append(lits, literal{pos: fset.Position(n.Pos()), value: v, link: args[n]})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lits) == 0 {
		t.Fatalf("no string literal found under %s", root)
	}
	return lits, bad
}

// A message, note or help line names a doc by DocLink, never by its path
// in the repository: an operator who runs a released tack has no
// checkout (#51).
func TestNoBareDocPaths(t *testing.T) {
	lits, _ := literals(t)
	for _, l := range lits {
		if !l.link && strings.Contains(l.value, "docs/") {
			t.Errorf("%s: %q names a doc by its path: use version.DocLink", l.pos, l.value)
		}
	}
}

// Each file and anchor that a DocLink call names exists in the
// repository, so the link of a release names a file of its tag.
func TestDocLinksExist(t *testing.T) {
	lits, bad := literals(t)
	for _, p := range bad {
		t.Errorf("%s: DocLink takes one string literal, so that this test can check it", p)
	}
	n := 0
	for _, l := range lits {
		if !l.link {
			continue
		}
		n++
		file, anchor, _ := strings.Cut(l.value, "#")
		if file == "" || filepath.IsAbs(file) || strings.Contains(file, "..") {
			t.Errorf("%s: %q is not a path from the repository's root", l.pos, l.value)
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Errorf("%s: %q: %v", l.pos, l.value, err)
			continue
		}
		if anchor != "" && !anchors(string(b))[anchor] {
			t.Errorf("%s: %q: %s has no heading with the anchor %q", l.pos, l.value, file, anchor)
		}
	}
	if n == 0 {
		t.Fatal("no DocLink call found")
	}
}

var heading = regexp.MustCompile(`^#{1,6}[ \t]+(.*?)[ \t#]*$`)

// anchors returns the anchors GitHub gives the Markdown headings of doc:
// the heading in lower case, without punctuation but - and _, with each
// space a -, and -1, -2 and so on after a repeated one. It covers headings
// of plain text, as the docs have.
func anchors(doc string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	fence := false
	sc := bufio.NewScanner(strings.NewReader(doc))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			continue
		}
		m := heading.FindStringSubmatch(line)
		if fence || m == nil {
			continue
		}
		var b strings.Builder
		for _, r := range strings.ToLower(m[1]) {
			switch {
			case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
				b.WriteRune(r)
			case r == ' ':
				b.WriteRune('-')
			}
		}
		a := b.String()
		if n := seen[a]; n > 0 {
			seen[a]++
			a += "-" + strconv.Itoa(n)
		} else {
			seen[a] = 1
		}
		out[a] = true
	}
	return out
}

func TestAnchors(t *testing.T) {
	got := anchors("# Fail over\n\n## When Tack refuses the lost site\n\n```sh\n# a comment\n```\n\n### Who may write ledgers\n\n## Steps\n\n## Steps\n")
	for _, want := range []string{"fail-over", "when-tack-refuses-the-lost-site", "who-may-write-ledgers", "steps", "steps-1"} {
		if !got[want] {
			t.Errorf("anchors: no %q in %v", want, got)
		}
	}
	if got["a-comment"] {
		t.Errorf("anchors: a comment in a code block is no heading: %v", got)
	}
}
