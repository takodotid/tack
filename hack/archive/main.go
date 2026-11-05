// Command archive writes a release archive, .tar.gz or .zip by the archive's name, that holds one directory, as
// `tar -C parent -czf` does. The same files give the same bytes, whatever the build's time, umask, owner or time zone
// ([RFC#0009] Requirement 18): every entry has the time SOURCE_DATE_EPOCH, owner 0, and mode 0755 for a directory or an
// executable file and 0644 otherwise; the gzip header holds no name or time. `make dist` uses it for every archive, so
// the build needs neither GNU tar nor a zip tool.
//
//	SOURCE_DATE_EPOCH=<seconds> go run ./hack/archive <archive.tar.gz|archive.zip> <dir>
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Getenv("SOURCE_DATE_EPOCH")); err != nil {
		fmt.Fprintln(os.Stderr, "archive:", err)
		os.Exit(1)
	}
}

func run(args []string, epoch string) error {
	if len(args) != 2 {
		return errors.New("usage: SOURCE_DATE_EPOCH=<seconds> archive <archive.tar.gz|archive.zip> <dir>")
	}
	archive, dir := args[0], filepath.Clean(args[1])
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil || secs < 0 {
		return fmt.Errorf("SOURCE_DATE_EPOCH %q is not a number of seconds", epoch)
	}
	mtime := time.Unix(secs, 0).UTC()
	var write func(io.Writer, []entry, time.Time) error
	switch {
	case strings.HasSuffix(archive, ".tar.gz"):
		write = writeTar
	case strings.HasSuffix(archive, ".zip"):
		write = writeZip
	default:
		return fmt.Errorf("%s: want a name ending in .tar.gz or .zip", archive)
	}
	entries, err := list(dir)
	if err != nil {
		return err
	}
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	err = write(f, entries, mtime)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(archive)
	}
	return err
}

// entry is a directory or a regular file to archive.
type entry struct {
	path string // the path on disk
	name string // the name in the archive, with forward slashes
	dir  bool
	mode int64 // 0755 or 0644
	size int64
}

// list returns dir and everything under it, in the order of filepath.WalkDir, which sorts each directory by name. It
// fails on anything but a directory or a regular file.
func list(dir string) ([]entry, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	parent := filepath.Dir(dir)
	var out []entry
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(parent, path)
		if err != nil {
			return err
		}
		e := entry{path: path, name: filepath.ToSlash(rel), mode: 0o644}
		switch {
		case d.IsDir():
			e.dir, e.mode = true, 0o755
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", path)
		default:
			e.size = info.Size()
			// The build's umask decides the other bits; only whether the file runs counts.
			if info.Mode().Perm()&0o111 != 0 {
				e.mode = 0o755
			}
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// writeTar writes a gzip-compressed tar with no name or time in the gzip header, and owner 0 for every entry.
func writeTar(w io.Writer, entries []entry, mtime time.Time) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, ModTime: mtime, Format: tar.FormatPAX}
		if e.dir {
			h.Typeflag, h.Name = tar.TypeDir, e.name+"/"
		} else {
			h.Typeflag, h.Size = tar.TypeReg, e.size
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if e.dir {
			continue
		}
		if err := copyFile(tw, e); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// writeZip writes a zip whose entries are deflated, except directories, and carry the time in UTC.
func writeZip(w io.Writer, entries []entry, mtime time.Time) error {
	zw := zip.NewWriter(w)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Modified: mtime, Method: zip.Deflate}
		mode := fs.FileMode(e.mode)
		if e.dir {
			h.Name, h.Method, mode = e.name+"/", zip.Store, mode|fs.ModeDir
		}
		h.SetMode(mode)
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if e.dir {
			continue
		}
		if err := copyFile(fw, e); err != nil {
			return err
		}
	}
	return zw.Close()
}

// copyFile copies the file of e into w, and fails when its size changed since list.
func copyFile(w io.Writer, e entry) error {
	src, err := os.Open(e.path)
	if err != nil {
		return err
	}
	defer src.Close()
	n, err := io.Copy(w, src)
	if err != nil {
		return err
	}
	if n != e.size {
		return fmt.Errorf("%s changed while it was archived", e.path)
	}
	return nil
}
