package version

import "testing"

// A release build links the docs at its tag. Every other build links
// main: git describe names a build after a tag, or a dirty one, with no
// tag of that name.
func TestRefOf(t *testing.T) {
	for v, want := range map[string]string{
		"v1.3.0":            "v1.3.0",
		"v10.20.30":         "v10.20.30",
		"dev":               "main",
		"":                  "main",
		"v1.2.1-5-g2961117": "main",
		"v1.2.1-dirty":      "main",
		"2961117":           "main",
		"2961117-dirty":     "main",
		"1.3.0":             "main",
		"v1.3":              "main",
		"v1.3.0-rc.1":       "main",
		"v1.3.0\n":          "main",
	} {
		if got := RefOf(v); got != want {
			t.Errorf("RefOf(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestDocLink(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	for v, want := range map[string]string{
		"v1.3.0":            "https://github.com/takodotid/tack/blob/v1.3.0/docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site",
		"dev":               "https://github.com/takodotid/tack/blob/main/docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site",
		"v1.2.1-5-g2961117": "https://github.com/takodotid/tack/blob/main/docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site",
	} {
		Version = v
		if got := DocLink("docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site"); got != want {
			t.Errorf("version %s: DocLink = %q, want %q", v, got, want)
		}
	}
}
