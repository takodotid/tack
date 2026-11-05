// Package version holds the binary's version, and links to the docs of
// that version ([RFC#0009] Requirement 17).
package version

import "regexp"

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

// Ref returns the Git ref of the docs of this binary.
func Ref() string { return RefOf(Version) }

// DocLink returns the link to path at Ref. path is a file from the
// repository's root, with its anchor if any, such as
// "docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site". Pass it
// as a string literal: a test checks that each one exists.
func DocLink(path string) string { return tree + Ref() + "/" + path }
