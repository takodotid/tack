package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// epoch is the SOURCE_DATE_EPOCH of the archives that writeDist writes.
const epoch = "1793000000"

var mtime = time.Unix(1793000000, 0).UTC()

// tweak changes the headers that writeDist writes, each one reproducible
// unless changed.
type tweak struct {
	tar func(*tar.Header)
	gz  func(*gzip.Header)
	zip func(*zip.FileHeader)
}

// writeDist writes dist/man and one archive per platform, holding files
// under the archive's directory.
func writeDist(t *testing.T, files func(goos string) []string, tw tweak) string {
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
			writeZip(t, filepath.Join(dist, name+".zip"), paths, tw.zip)
		} else {
			writeTar(t, filepath.Join(dist, name+".tar.gz"), paths, tw.tar, tw.gz)
		}
	}
	return dist
}

func writeTar(t *testing.T, archive string, paths []string, change func(*tar.Header), changeGz func(*gzip.Header)) {
	t.Helper()
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if changeGz != nil {
		changeGz(&gz.Header)
	}
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		h := &tar.Header{Name: p, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg, ModTime: mtime}
		if change != nil {
			change(h)
		}
		if err := tw.WriteHeader(h); err != nil {
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

func writeZip(t *testing.T, archive string, paths []string, change func(*zip.FileHeader)) {
	t.Helper()
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, p := range paths {
		h := &zip.FileHeader{Name: p, Method: zip.Deflate, Modified: mtime}
		h.SetMode(0o644)
		if change != nil {
			change(h)
		}
		w, err := zw.CreateHeader(h)
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
			err := run(append([]string{writeDist(t, tt.files, tweak{}), "v9"}, platforms...), epoch)
			switch {
			case tt.err == "" && err != nil:
				t.Errorf("run: %v", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Errorf("run: %v, want %q", err, tt.err)
			}
		})
	}
	// An archive that is not there.
	dist := writeDist(t, complete, tweak{})
	if err := run([]string{dist, "v9", "darwin/arm64"}, epoch); err == nil || !strings.Contains(err.Error(), "tack-v9-darwin-arm64.tar.gz") {
		t.Errorf("a missing archive: %v", err)
	}
}

// An entry that the build's time, owner, umask or time zone would change
// fails make dist ([RFC#0009] Requirement 18). Found in a review (#35,
// row 44): tar kept the build's time and owner.
func TestRunRefusesUnreproducibleArchives(t *testing.T) {
	platforms := []string{"linux/amd64", "windows/amd64"}
	if err := run(append([]string{writeDist(t, complete, tweak{}), "v9"}, platforms...), epoch); err != nil {
		t.Fatalf("reproducible archives: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	for _, tt := range []struct {
		name string
		tw   tweak
		err  string
	}{
		{"tar: the build's time", tweak{tar: func(h *tar.Header) { h.ModTime = now }}, "has the time"},
		{"tar: the builder's user", tweak{tar: func(h *tar.Header) { h.Uid, h.Uname = 1000, "builder" }}, "has the owner 1000:0"},
		{"tar: the builder's group", tweak{tar: func(h *tar.Header) { h.Gid = 1000 }}, "has the owner 0:1000"},
		{"tar: a group-writable file", tweak{tar: func(h *tar.Header) { h.Mode = 0o664 }}, "has the mode 664"},
		{"gzip: the build's time", tweak{gz: func(h *gzip.Header) { h.ModTime = now }}, "the gzip header holds"},
		{"gzip: a file name", tweak{gz: func(h *gzip.Header) { h.Name = "tack.tar" }}, "the gzip header holds the name"},
		{"zip: the build's time", tweak{zip: func(h *zip.FileHeader) { h.Modified = now }}, "has the time"},
		{"zip: another time zone", tweak{zip: func(h *zip.FileHeader) { h.Modified = mtime.In(time.FixedZone("UTC+7", 7*3600)) }}, "has the time"},
		{"zip: a group-writable file", tweak{zip: func(h *zip.FileHeader) { h.SetMode(0o664) }}, "has the mode 664"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := run(append([]string{writeDist(t, complete, tt.tw), "v9"}, platforms...), epoch)
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("run: %v, want %q", err, tt.err)
			}
		})
	}
	if err := run(append([]string{writeDist(t, complete, tweak{}), "v9"}, platforms...), ""); err == nil {
		t.Error("run without SOURCE_DATE_EPOCH: no error")
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
