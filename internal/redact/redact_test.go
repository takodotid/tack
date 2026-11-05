package redact

import "testing"

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		"fetch https://user:secret@git.example.invalid/r.git failed": "fetch https://***@git.example.invalid/r.git failed",
		"https://tok3n@git.example.invalid/r.git":                    "https://***@git.example.invalid/r.git",
		"https://user:p@ss@git.example.invalid/r.git":                "https://***@git.example.invalid/r.git",
		"ssh://git@git.example.invalid/r.git":                        "ssh://***@git.example.invalid/r.git",
		"git@git.example.invalid:r.git":                              "git@git.example.invalid:r.git",
		"https://git.example.invalid/a@b":                            "https://git.example.invalid/a@b",
		"two: http://a:b@x/1 and https://c:d@y/2":                    "two: http://***@x/1 and https://***@y/2",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}
