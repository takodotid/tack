// Command distcheck lists each release archive that `make dist` wrote and
// checks its contents ([RFC#0009] Requirement 12): one directory named
// after the archive, with the binary, every man page in dist/man under
// man/man1/, the CRD, LICENSE and README.md, and nothing else. Each link
// to the docs in the man pages names the ref of the version
// ([RFC#0009] Requirement 17).
//
//	go run ./hack/distcheck <dist> <version> <os/arch>...
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	tackversion "github.com/takodotid/tack/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "distcheck:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 3 {
		return errors.New("usage: distcheck <dist> <version> <os/arch>...")
	}
	dist, version, platforms := args[0], args[1], args[2:]
	man, err := filepath.Glob(filepath.Join(dist, "man", "*.1"))
	if err != nil {
		return err
	}
	pages := map[string]bool{}
	for _, m := range man {
		pages[filepath.Base(m)] = true
	}
	if !pages["tack.1"] || len(pages) < 2 {
		return fmt.Errorf("%s holds %d man pages; want tack.1 and one per subcommand", filepath.Join(dist, "man"), len(pages))
	}
	if err := checkLinks(man, version); err != nil {
		return err
	}
	var errs []error
	for _, p := range platforms {
		goos, goarch, ok := strings.Cut(p, "/")
		if !ok {
			return fmt.Errorf("platform %q is not os/arch", p)
		}
		if err := check(dist, version, goos, goarch, pages); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Printf("distcheck: %s-%s: ok\n", goos, goarch)
	}
	return errors.Join(errs...)
}

// docLink matches a link to the repository's files, and its ref.
var docLink = regexp.MustCompile(`https://github\.com/takodotid/tack/blob/([^/\s]+)/`)

// checkLinks checks that each link to the docs in the man pages names the
// ref of version. genman writes them apart from the binary, with the
// version of its own -ldflags.
func checkLinks(man []string, version string) error {
	want := tackversion.RefOf(version)
	for _, m := range man {
		b, err := os.ReadFile(m)
		if err != nil {
			return err
		}
		for _, l := range docLink.FindAllStringSubmatch(string(b), -1) {
			if l[1] != want {
				return fmt.Errorf("%s links the docs at %s, want %s for version %s: build the man pages with the binary's -ldflags", m, l[1], want, version)
			}
		}
	}
	return nil
}

// expected lists the files of an archive.
func expected(name, goos string, pages map[string]bool) []string {
	bin := "tack"
	if goos == "windows" {
		bin = "tack.exe"
	}
	out := []string{bin, "LICENSE", "README.md", "failover.tako.id_sitefailovers.yaml"}
	for p := range pages {
		out = append(out, "man/man1/"+p)
	}
	for i := range out {
		out[i] = name + "/" + out[i]
	}
	slices.Sort(out)
	return out
}

func check(dist, version, goos, goarch string, pages map[string]bool) error {
	name := fmt.Sprintf("tack-%s-%s-%s", version, goos, goarch)
	archive := filepath.Join(dist, name+".tar.gz")
	list := listTar
	if goos == "windows" {
		archive, list = filepath.Join(dist, name+".zip"), listZip
	}
	files, err := list(archive)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	slices.Sort(files)
	want := expected(name, goos, pages)
	var missing, extra []string
	for _, w := range want {
		if !slices.Contains(files, w) {
			missing = append(missing, w)
		}
	}
	for _, f := range files {
		if !slices.Contains(want, f) {
			extra = append(extra, f)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("%s: missing %v, unexpected %v", archive, missing, extra)
	}
	return nil
}

// listTar returns the regular files of a .tar.gz, and fails on an entry
// outside one top directory.
func listTar(archive string) ([]string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if err := inOneDir(archive, h.Name); err != nil {
			return nil, err
		}
		switch h.Typeflag {
		case tar.TypeReg:
			out = append(out, path.Clean(h.Name))
		case tar.TypeDir:
		default:
			return nil, fmt.Errorf("entry %s is neither a file nor a directory", h.Name)
		}
	}
}

// listZip returns the files of a .zip, and fails on an entry outside one
// top directory.
func listZip(archive string) ([]string, error) {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []string
	for _, f := range r.File {
		if err := inOneDir(archive, f.Name); err != nil {
			return nil, err
		}
		if !f.FileInfo().IsDir() {
			out = append(out, path.Clean(f.Name))
		}
	}
	return out, nil
}

// inOneDir checks that an entry lies under the directory named after the
// archive.
func inOneDir(archive, entry string) error {
	base := filepath.Base(archive)
	dir := strings.TrimSuffix(strings.TrimSuffix(base, ".tar.gz"), ".zip")
	if e := path.Clean(entry); e != dir && !strings.HasPrefix(e, dir+"/") {
		return fmt.Errorf("entry %s is outside the directory %s", entry, dir)
	}
	return nil
}
