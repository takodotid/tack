package flux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// bigCheckout copies testdata/repo to a new directory, and adds files that
// no Kustomization reads, up to about size bytes in all.
func bigCheckout(b *testing.B, size int) string {
	b.Helper()
	dir := b.TempDir()
	if err := copySource(repoDir, dir, "", false); err != nil {
		b.Fatal(err)
	}
	const file = 100 << 10
	data := make([]byte, file)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	for i := 0; i*file < size; i++ {
		p := filepath.Join(dir, "apps", fmt.Sprintf("team-%02d", i%50), fmt.Sprintf("manifest-%04d.yaml", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

// BenchmarkRender measures the render of one Kustomization of a checkout
// of 10 MB and of 100 MB (#35 row 47): VerifyGit renders each
// Kustomization that applies a held object, at each site, and each render
// copies the checkout. copy is the copy alone.
func BenchmarkRender(b *testing.B) {
	ctx := context.Background()
	for _, mb := range []int{10, 100} {
		dir := bigCheckout(b, mb<<20)
		b.Run(fmt.Sprintf("checkout=%dMB/copy", mb), func(b *testing.B) {
			b.SetBytes(int64(mb << 20))
			for b.Loop() {
				dst := b.TempDir()
				if err := copySource(dir, dst, "", false); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("checkout=%dMB/render", mb), func(b *testing.B) {
			ks := renderKustomization("./clusters/a")
			c := renderClient()
			for b.Loop() {
				if _, err := render(ctx, c, ks, dir, []failoverv1.ObjectRef{clusterRef, poolerRef}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
