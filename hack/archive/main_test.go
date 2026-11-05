package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const epoch = "1793000000"

var files = map[string]string{
	"tack":                                "binary",
	"failover.tako.id_sitefailovers.yaml": "kind: CustomResourceDefinition\n",
	"man/man1/tack.1":                     ".TH tack\n",
	"man/man1/tack-plan.1":                ".TH tack-plan\n",
}

// writeDir writes files under tmp/name, with the file mode and time given, and returns the directory.
func writeDir(t *testing.T, tmp, name string, mode os.FileMode, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(tmp, name)
	for f, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		m := mode
		if f == "tack" {
			m |= 0o100
		}
		if err := os.WriteFile(p, []byte(content), m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	err := filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, mtime, mtime)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// got is an entry of an archive: its mode, time and content.
type got struct {
	mode    int64
	mtime   time.Time
	content string
}

func readTar(t *testing.T, archive string) map[string]got {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header: name %q, time %v; want neither", gz.Name, gz.ModTime)
	}
	out := map[string]got{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: owner %d:%d %q:%q, want 0:0", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = got{h.Mode, h.ModTime.UTC(), string(b)}
	}
}

func readZip(t *testing.T, archive string) map[string]got {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]got{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !f.FileInfo().IsDir() && f.Method != zip.Deflate {
			t.Errorf("%s: method %d, want deflate", f.Name, f.Method)
		}
		out[f.Name] = got{int64(f.Mode().Perm()), f.Modified.UTC(), string(b)}
	}
	return out
}

// The archive holds the directory itself and every file under it with its content, under names with forward slashes,
// each with the time SOURCE_DATE_EPOCH and mode 0755 or 0644.
func TestWrite(t *testing.T) {
	want := time.Unix(1793000000, 0).UTC()
	for _, ext := range []string{".tar.gz", ".zip"} {
		t.Run(ext, func(t *testing.T) {
			tmp := t.TempDir()
			dir := writeDir(t, tmp, "tack-v1.3.0-linux-amd64", 0o664, time.Now())
			archive := filepath.Join(tmp, "out"+ext)
			if err := run([]string{archive, dir + string(filepath.Separator)}, epoch); err != nil {
				t.Fatal(err)
			}
			entries := map[string]got{}
			if ext == ".zip" {
				entries = readZip(t, archive)
			} else {
				entries = readTar(t, archive)
			}
			var names []string
			for name, e := range entries {
				names = append(names, name)
				if !e.mtime.Equal(want) {
					t.Errorf("%s: time %v, want %v", name, e.mtime, want)
				}
			}
			slices.Sort(names)
			wantNames := []string{
				"tack-v1.3.0-linux-amd64/",
				"tack-v1.3.0-linux-amd64/failover.tako.id_sitefailovers.yaml",
				"tack-v1.3.0-linux-amd64/man/",
				"tack-v1.3.0-linux-amd64/man/man1/",
				"tack-v1.3.0-linux-amd64/man/man1/tack-plan.1",
				"tack-v1.3.0-linux-amd64/man/man1/tack.1",
				"tack-v1.3.0-linux-amd64/tack",
			}
			if !slices.Equal(names, wantNames) {
				t.Errorf("entries:\n got %q\nwant %q", names, wantNames)
			}
			for name, content := range files {
				e := entries["tack-v1.3.0-linux-amd64/"+name]
				if e.content != content {
					t.Errorf("%s: got %q, want %q", name, e.content, content)
				}
				mode := int64(0o644)
				if name == "tack" {
					mode = 0o755
				}
				if e.mode != mode {
					t.Errorf("%s: mode %o, want %o", name, e.mode, mode)
				}
			}
			if e := entries["tack-v1.3.0-linux-amd64/man/"]; e.mode != 0o755 {
				t.Errorf("man/: mode %o, want 755", e.mode)
			}
		})
	}
}

// The same files give the same bytes, whatever their time, the umask that set their mode, and the time zone
// ([RFC#0009] Requirement 18). Found in a review (#35, row 44): the archives kept the build's time and owner.
func TestReproducible(t *testing.T) {
	for _, ext := range []string{".tar.gz", ".zip"} {
		t.Run(ext, func(t *testing.T) {
			tmp := t.TempDir()
			a := writeDir(t, filepath.Join(tmp, "a"), "tack-v1.3.0-linux-amd64", 0o644, time.Unix(1700000000, 0))
			b := writeDir(t, filepath.Join(tmp, "b"), "tack-v1.3.0-linux-amd64", 0o660, time.Now())
			if err := run([]string{filepath.Join(tmp, "a"+ext), a}, epoch); err != nil {
				t.Fatal(err)
			}
			tz := time.Local
			time.Local = time.FixedZone("UTC+7", 7*3600)
			defer func() { time.Local = tz }()
			if err := run([]string{filepath.Join(tmp, "b"+ext), b}, epoch); err != nil {
				t.Fatal(err)
			}
			x, err := os.ReadFile(filepath.Join(tmp, "a"+ext))
			if err != nil {
				t.Fatal(err)
			}
			y, err := os.ReadFile(filepath.Join(tmp, "b"+ext))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(x, y) {
				t.Errorf("two archives of the same files differ: %d and %d bytes", len(x), len(y))
			}
		})
	}
}

// A failed write leaves no archive behind.
func TestWriteFails(t *testing.T) {
	tmp := t.TempDir()
	dir := writeDir(t, tmp, "tack-v1.3.0-linux-amd64", 0o644, time.Now())
	for name, tt := range map[string]struct {
		args  []string
		epoch string
	}{
		"missing directory": {[]string{filepath.Join(tmp, "out.zip"), filepath.Join(tmp, "missing")}, epoch},
		"usage":             {[]string{filepath.Join(tmp, "out.zip")}, epoch},
		"no epoch":          {[]string{filepath.Join(tmp, "out.tar.gz"), dir}, ""},
		"bad epoch":         {[]string{filepath.Join(tmp, "out.tar.gz"), dir}, "yesterday"},
		"other format":      {[]string{filepath.Join(tmp, "out.tar"), dir}, epoch},
	} {
		if err := run(tt.args, tt.epoch); err == nil {
			t.Errorf("%s: no error", name)
		}
		for _, f := range []string{"out.zip", "out.tar.gz", "out.tar"} {
			if _, err := os.Stat(filepath.Join(tmp, f)); !os.IsNotExist(err) {
				t.Errorf("%s: %s left behind: %v", name, f, err)
			}
		}
	}
}
