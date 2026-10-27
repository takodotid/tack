package flux

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
)

const (
	repoDir = "testdata/repo"
	secret  = "s3cr3t-value"
)

// renderKustomization applies ./clusters/a with a Flux patch, a target
// namespace, substitutions from inline values and a Secret, and common
// labels.
func renderKustomization(path string) *unstructured.Unstructured {
	return object(KustomizationGVK, ksKey.Namespace, ksKey.Name, map[string]any{
		"spec": map[string]any{
			"path":            path,
			"sourceRef":       map[string]any{"kind": "GitRepository", "name": "flux-system"},
			"targetNamespace": "db",
			"patches": []any{map[string]any{
				"patch":  "- op: replace\n  path: /spec/instances\n  value: 0\n",
				"target": map[string]any{"kind": "Pooler"},
			}},
			"postBuild": map[string]any{
				"substitute":     map[string]any{"site": "a"},
				"substituteFrom": []any{map[string]any{"kind": "Secret", "name": "db-vars"}},
			},
			"commonMetadata": map[string]any{"labels": map[string]any{"team": "data"}},
		},
	})
}

func varsSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ksKey.Namespace, Name: "db-vars"},
		Data:       map[string][]byte{"token": []byte(secret)},
	}
}

func renderClient(extra ...client.Object) client.Client {
	objs := append([]client.Object{gitRepository(), varsSecret()}, extra...)
	return newClient(nil, objs...)
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRender(t *testing.T) {
	ctx := context.Background()
	before := snapshot(t, repoDir)
	ks := renderKustomization("./clusters/a")
	r, err := render(ctx, renderClient(), ks, repoDir, []v1alpha1.ObjectRef{clusterRef, poolerRef})
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, repoDir); len(after) != len(before) {
		t.Errorf("render changed the checkout: %d files, want %d", len(after), len(before))
	} else {
		for p, b := range before {
			if after[p] != b {
				t.Errorf("render changed %s", p)
			}
		}
	}

	cl, ok := r.Objects[refID(clusterRef)]
	if !ok {
		t.Fatalf("no Cluster db/pg in %v", r.Objects)
	}
	for _, c := range []struct {
		field string
		want  any
	}{
		{"metadata.namespace", "db"},
		{"spec.replica.primary", "pg-b"},
		{"spec.replica.source", "pg-a"},
		{"spec.instances", int64(3)},
		{"spec.description", "token " + secret},
		{"metadata.labels.team", "data"},
	} {
		p, _ := parsePath(c.field)
		if got, _ := lookup(cl.Object, p); got != c.want {
			t.Errorf("Cluster %s = %#v, want %#v", c.field, got, c.want)
		}
	}
	labels := cl.GetLabels()
	if labels[LabelKustomizationName] != "db" || labels[LabelKustomizationNamespace] != "flux-system" {
		t.Errorf("owner labels %v", labels)
	}
	if v := cl.GetAnnotations()["example.org/site"]; v != "a" {
		t.Errorf("inline substitution: %q", v)
	}
	po, ok := r.Objects[refID(poolerRef)]
	if !ok {
		t.Fatal("no Pooler db/pg-rw")
	}
	if got, _, _ := unstructured.NestedInt64(po.Object, "spec", "instances"); got != 0 {
		t.Errorf("Pooler spec.instances = %d, want 0 from the Flux patch", got)
	}
	if got := r.Mask.mask("value " + secret + " here"); strings.Contains(got, secret) {
		t.Errorf("mask left the Secret value: %q", got)
	}
}

func TestRenderErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("path", func(t *testing.T) {
		_, err := render(ctx, renderClient(), renderKustomization("./clusters/b"), repoDir, []v1alpha1.ObjectRef{clusterRef})
		if err == nil || !strings.Contains(err.Error(), "not in the checkout") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("build", func(t *testing.T) {
		_, err := render(ctx, renderClient(), renderKustomization("./clusters/broken"), repoDir, []v1alpha1.ObjectRef{clusterRef})
		if err == nil || !strings.Contains(err.Error(), "without remote bases") || !strings.Contains(err.Error(), "missing.yaml") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("OCI source", func(t *testing.T) {
		ks := renderKustomization("./clusters/a")
		_ = unstructured.SetNestedField(ks.Object, "OCIRepository", "spec", "sourceRef", "kind")
		_, err := render(ctx, renderClient(), ks, repoDir, []v1alpha1.ObjectRef{clusterRef})
		if err == nil || !strings.Contains(err.Error(), "not from a GitRepository") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing variable", func(t *testing.T) {
		ks := renderKustomization("./clusters/a")
		unstructured.RemoveNestedField(ks.Object, "spec", "postBuild", "substituteFrom")
		_, err := render(ctx, renderClient(), ks, repoDir, []v1alpha1.ObjectRef{clusterRef})
		if err == nil || !strings.Contains(err.Error(), `variable not set (strict mode): "token"`) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("includes", func(t *testing.T) {
		repo := gitRepository()
		_ = unstructured.SetNestedSlice(repo.Object, []any{map[string]any{"repository": map[string]any{"name": "other"}}}, "spec", "include")
		c := newClient(nil, repo, varsSecret())
		_, err := render(ctx, c, renderKustomization("./clusters/a"), repoDir, []v1alpha1.ObjectRef{clusterRef})
		if err == nil || !strings.Contains(err.Error(), "includes other repositories") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCopySource(t *testing.T) {
	write := func(t *testing.T, root string, files ...string) {
		t.Helper()
		for _, f := range files {
			p := filepath.Join(root, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x: 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	list := func(t *testing.T, root string) []string {
		t.Helper()
		var out []string
		err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				rel, _ := filepath.Rel(root, p)
				out = append(out, rel)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}

	t.Run("default rules", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		write(t, src, "a.yaml", "sub/b.yaml", ".git/config", ".github/workflow.yaml", "logo.png")
		if err := os.Symlink("a.yaml", filepath.Join(src, "link.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := copySource(src, dst, "", false); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(list(t, dst), ","), "a.yaml,sub/b.yaml"; got != want {
			t.Errorf("copied %s, want %s", got, want)
		}
	})
	t.Run("ignore rules", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		write(t, src, "a.yaml", "skip/c.yaml", "extra.yaml", "logo.png", ".git/config")
		if err := os.WriteFile(filepath.Join(src, ".sourceignore"), []byte("skip/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := copySource(src, dst, "extra.yaml\n", true); err != nil {
			t.Fatal(err)
		}
		// With rules of its own, source-controller drops only VCS files by default.
		if got, want := strings.Join(list(t, dst), ","), ".sourceignore,a.yaml,logo.png"; got != want {
			t.Errorf("copied %s, want %s", got, want)
		}
	})
}
