package gitops

import (
	"context"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// history writes a bare repository whose branch main has commits commits:
// each changes the image tag in 3 of files files of about 2 KB, as
// releases through GitOps do. It uses git fast-import.
func history(b *testing.B, commits, files int) string {
	b.Helper()
	dir := filepath.Join(b.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", dir).CombinedOutput(); err != nil {
		b.Fatalf("git init: %v\n%s", err, out)
	}
	var stream strings.Builder
	pad := strings.Repeat("  # a line of a manifest that does not change between releases\n", 30)
	file := func(i, tag int) string {
		return fmt.Sprintf("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app-%03d\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        image: registry.example.com/app-%03d:v1.%d.0\n%s", i, i, tag, pad)
	}
	for c := 1; c <= commits; c++ {
		msg := fmt.Sprintf("release %d", c)
		fmt.Fprintf(&stream, "commit refs/heads/main\nmark :%d\ncommitter Tester <tester@example.invalid> %d +0000\ndata %d\n%s\n", c, 1700000000+c*60, len(msg), msg)
		if c > 1 {
			fmt.Fprintf(&stream, "from :%d\n", c-1)
		}
		changed := []int{c % files, (c * 7) % files, (c * 13) % files}
		if c == 1 {
			changed = nil
			for i := 0; i < files; i++ {
				changed = append(changed, i)
			}
		}
		for _, i := range changed {
			body := file(i, c)
			fmt.Fprintf(&stream, "M 100644 inline apps/app-%03d/deployment.yaml\ndata %d\n%s\n", i, len(body), body)
		}
	}
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stream.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git fast-import: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", dir, "repack", "-adq").CombinedOutput(); err != nil {
		b.Fatalf("git repack: %v\n%s", err, out)
	}
	return dir
}

func size(b *testing.B, dir string, skip string) int64 {
	b.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == skip {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			n += fi.Size()
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	return n
}

// BenchmarkFirstFetch measures the fetch that each tack process makes
// first, of the branch's whole history (#35 row 47), on a repository of
// 5,000 releases over 200 manifests: the bytes it stores, which a fetch
// over the network transfers, against the files of the branch's head.
// The process's later fetches bring only new commits. Run it with
// -benchtime=1x.
func BenchmarkFirstFetch(b *testing.B) {
	b.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	b.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	b.Setenv("HOME", b.TempDir())
	origin := history(b, 5000, 200)
	w := workload("file://" + origin)
	var objects, tree int64
	for b.Loop() {
		r := New(b.TempDir())
		head, err := r.BranchHead(context.Background(), w)
		if err != nil {
			b.Fatal(err)
		}
		dir, err := r.Checkout(context.Background(), w, head)
		if err != nil {
			b.Fatal(err)
		}
		for _, c := range r.clones {
			objects = size(b, filepath.Join(c.dir, ".git", "objects"), "")
		}
		tree = size(b, dir, ".git")
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(objects), "history-bytes")
	b.ReportMetric(float64(tree), "head-tree-bytes")
	b.ReportMetric(float64(size(b, origin, "")), "origin-bytes")
}
