package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDist writes dist/man and one archive per platform, holding files
// under the archive's directory.
func writeDist(t *testing.T, files func(goos string) []string) string {
	t.Helper()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "man"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"tack.1", "tack-plan.1"} {
		if err := os.WriteFile(filepath.Join(dist, "man", p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, goos := range []string{"linux", "windows"} {
		name := "tack-v9-" + goos + "-amd64"
		var paths []string
		for _, f := range files(goos) {
			paths = append(paths, name+"/"+f)
		}
		if goos == "windows" {
			writeZip(t, filepath.Join(dist, name+".zip"), paths)
		} else {
			writeTar(t, filepath.Join(dist, name+".tar.gz"), paths)
		}
	}
	return dist
}

func writeTar(t *testing.T, archive string, paths []string) {
	t.Helper()
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func writeZip(t *testing.T, archive string, paths []string) {
	t.Helper()
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, p := range paths {
		w, err := zw.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func complete(goos string) []string {
	bin := "tack"
	if goos == "windows" {
		bin = "tack.exe"
	}
	return []string{bin, "LICENSE", "README.md", "failover.tako.id_sitefailovers.yaml", "man/man1/tack.1", "man/man1/tack-plan.1"}
}

func TestRun(t *testing.T) {
	platforms := []string{"linux/amd64", "windows/amd64"}
	for _, tt := range []struct {
		name  string
		files func(goos string) []string
		err   string
	}{
		{name: "complete", files: complete},
		{name: "a man page missing", err: "missing [tack-v9-linux-amd64/man/man1/tack-plan.1]",
			files: func(goos string) []string {
				f := complete(goos)
				if goos == "linux" {
					f = f[:len(f)-1]
				}
				return f
			}},
		{name: "a file too many", err: "unexpected [tack-v9-windows-amd64/notes.txt]",
			files: func(goos string) []string {
				f := complete(goos)
				if goos == "windows" {
					f = append(f, "notes.txt")
				}
				return f
			}},
		{name: "the Windows binary without .exe", err: "missing [tack-v9-windows-amd64/tack.exe], unexpected [tack-v9-windows-amd64/tack]",
			files: func(goos string) []string {
				f := complete(goos)
				f[0] = "tack"
				return f
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := run(append([]string{writeDist(t, tt.files), "v9"}, platforms...))
			switch {
			case tt.err == "" && err != nil:
				t.Errorf("run: %v", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Errorf("run: %v, want %q", err, tt.err)
			}
		})
	}
	// An archive that is not there.
	dist := writeDist(t, complete)
	if err := run([]string{dist, "v9", "darwin/arm64"}); err == nil || !strings.Contains(err.Error(), "tack-v9-darwin-arm64.tar.gz") {
		t.Errorf("a missing archive: %v", err)
	}
}

// The man pages of a release link the docs at its tag; of any other
// build, at main.
func TestCheckLinks(t *testing.T) {
	page := func(ref string) string {
		p := filepath.Join(t.TempDir(), "tack-run.1")
		s := "confirm the loss first:\nhttps://github.com/takodotid/tack/blob/" + ref + "/docs/runbooks/site-loss.md\n"
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tt := range []struct {
		version, ref, err string
	}{
		{version: "v1.3.0", ref: "v1.3.0"},
		{version: "v1.2.1-5-g2961117", ref: "main"},
		{version: "v1.3.0", ref: "main", err: "links the docs at main, want v1.3.0 for version v1.3.0"},
		{version: "v1.2.1-5-g2961117", ref: "v1.2.1", err: "links the docs at v1.2.1, want main"},
	} {
		err := checkLinks([]string{page(tt.ref)}, tt.version)
		if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("version %s, link at %s: %v, want %q", tt.version, tt.ref, err, tt.err)
		}
	}
}
