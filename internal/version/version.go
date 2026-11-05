// Package version holds the binary's version, and links to the docs of
// that version ([RFC#0009] Requirement 17).
package version

import (
	"regexp"
	"strconv"
)

// Version is set at build time:
// -X github.com/takodotid/tack/internal/version.Version=VERSION.
var Version = "dev"

// tree is where DocLink links: the repository's files at a ref.
const tree = "https://github.com/takodotid/tack/blob/"

// release matches a release tag. git describe also names builds after
// one, such as v1.2.1-5-g2961117 or v1.2.1-dirty: no tag holds those.
var release = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// RefOf returns the Git ref of the docs of version v: v when it is a
// release tag, such as v1.3.0, and main otherwise.
func RefOf(v string) string {
	if release.MatchString(v) {
		return v
	}
	return "main"
}

// described matches a release tag, or git describe's name of a build after
// one, such as v1.2.1-5-g2961117, with -dirty for a tree with changes.
var described = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9]+)-g[0-9a-f]+)?(?:-dirty)?$`)

// order returns the release and the commits after it that v names, and
// whether v names one.
func order(v string) ([4]int, bool) {
	m := described.FindStringSubmatch(v)
	if m == nil {
		return [4]int{}, false
	}
	var o [4]int
	for i := range o {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [4]int{}, false
		}
		o[i] = n
	}
	return o, true
}

// Newer tells whether version a is newer than version b ([RFC#0003]
// Requirement 24). Each must name a release tag, or a build after one as
// git describe names it; a build after a release is newer than the
// release and older than the next. When either names neither, such as
// dev or v1.3.0-rc.1, neither is newer.
func Newer(a, b string) bool {
	oa, okA := order(a)
	ob, okB := order(b)
	if !okA || !okB {
		return false
	}
	for i := range oa {
		if oa[i] != ob[i] {
			return oa[i] > ob[i]
		}
	}
	return false
}

// Ref returns the Git ref of the docs of this binary.
func Ref() string { return RefOf(Version) }

// DocLink returns the link to path at Ref. path is a file from the
// repository's root, with its anchor if any, such as
// "docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site". Pass it
// as a string literal: a test checks that each one exists.
func DocLink(path string) string { return tree + Ref() + "/" + path }
