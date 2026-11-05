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

// A ledger records the version of the tack that last wrote it, and an
// older release refuses to write it ([RFC#0003] Requirement 24). Versions
// that name no release are never newer.
func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1.3.1", "v1.3.0", true},
		{"v1.4.0", "v1.3.9", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.10.0", "v1.9.0", true},
		{"v1.3.0", "v1.3.0", false},
		{"v1.3.0", "v1.3.1", false},
		{"v1.2.9", "v1.3.0", false},
		{"v1.3.0-5-g2961117", "v1.3.0", true},
		{"v1.3.0", "v1.3.0-5-g2961117", false},
		{"v1.3.1", "v1.3.0-5-g2961117", true},
		{"v1.3.0-6-gabcdef0", "v1.3.0-5-g2961117", true},
		{"v1.3.0-dirty", "v1.3.0", false},
		{"v1.3.0-5-g2961117-dirty", "v1.3.0", true},
		{"v1.4.0", "dev", false},
		{"dev", "v1.3.0", false},
		{"v1.4.0", "", false},
		{"", "v1.3.0", false},
		{"v1.4.0-rc.1", "v1.3.0", false},
		{"v1.4.0", "v1.3.0-rc.1", false},
		{"2961117", "v1.3.0", false},
		{"v1.4.0\n", "v1.3.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
