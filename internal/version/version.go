// Package version holds the binary's version, and links to the docs of
// that version ([RFC#0009] Requirement 17).
package version

import (
	"regexp"
	"runtime/debug"
	"strconv"
)

// Version is set at build time:
// -X github.com/takodotid/tack/internal/version.Version=VERSION. A build
// without it, such as one of go install, takes the module version that Go
// recorded in the binary, when that names a release or a build after one
// (FromModule). Otherwise it stays dev.
var Version = "dev"

func init() { Version = resolve(Version, debug.ReadBuildInfo) }

// resolve returns the binary's version: v, as -X set it, or else, for a
// v of dev, the module version that read gives, when FromModule takes it.
func resolve(v string, read func() (*debug.BuildInfo, bool)) string {
	if v != "dev" {
		return v
	}
	if info, ok := read(); ok {
		return FromModule(info.Main.Version, v)
	}
	return v
}

// pseudo matches the version Go records for a build of a commit after the
// release vX.Y.Z, such as v1.4.1-0.20261111105842-d483d80abcde: the next
// patch, then 0, the commit's time and its hash. Go adds +dirty for a
// tree with changes.
var pseudo = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)-0\.[0-9]{14}-[0-9a-f]{12}(?:\+dirty)?$`)

// FromModule returns the version of a build from the module version that
// Go recorded in it: a release vX.Y.Z, such as go install
// github.com/takodotid/tack/cmd/tack@v1.4.0 records; a release with
// +dirty; or a build after a release, as pseudo matches. It returns
// fallback for any other, such as (devel), which go run, go test and a
// build without version control record.
func FromModule(mod, fallback string) string {
	if _, ok := order(mod); ok {
		return mod
	}
	return fallback
}

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
// one, such as v1.2.1-5-g2961117, with -dirty for a tree with changes, or
// a release with Go's +dirty.
var described = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9]+)-g[0-9a-f]+)?(?:-dirty|\+dirty)?$`)

// order returns the release and the commits after it that v names, and
// whether v names one. A build after a release that Go names by a
// pseudo-version counts one commit after it: Go records no count.
func order(v string) ([4]int, bool) {
	if m := pseudo.FindStringSubmatch(v); m != nil {
		var o [4]int
		for i := range 3 {
			n, err := strconv.Atoi(m[i+1])
			if err != nil {
				return [4]int{}, false
			}
			o[i] = n
		}
		if o[2] == 0 {
			// Go names a build after vX.Y.Z by vX.Y.(Z+1): a patch 0 here
			// comes after no release.
			return [4]int{}, false
		}
		o[2]--
		o[3] = 1
		return o, true
	}
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
// Requirement 24). A version names a release tag, or a build after one as
// git describe or Go names it; a build after a release is newer than the
// release and older than the next. A version that names neither, such as
// dev or v1.3.0-rc.1, is older than every one that does (#63): a tack of
// such a build cannot tell which fields a release's ledger holds. Of two
// such versions, neither is newer.
func Newer(a, b string) bool {
	oa, okA := order(a)
	ob, okB := order(b)
	switch {
	case !okA:
		return false
	case !okB:
		return true
	}
	for i := range oa {
		if oa[i] != ob[i] {
			return oa[i] > ob[i]
		}
	}
	return false
}

// Named tells whether v names a release, or a build after one.
func Named(v string) bool {
	_, ok := order(v)
	return ok
}

// Ref returns the Git ref of the docs of this binary.
func Ref() string { return RefOf(Version) }

// DocLink returns the link to path at Ref. path is a file from the
// repository's root, with its anchor if any, such as
// "docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site". Pass it
// as a string literal: a test checks that each one exists.
func DocLink(path string) string { return tree + Ref() + "/" + path }
