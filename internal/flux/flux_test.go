package flux

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/stop"
)

var (
	clusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	poolerGVK  = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"}
	clusterRef = failoverv1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Namespace: "db", Name: "pg"}
	poolerRef  = failoverv1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Pooler", Namespace: "db", Name: "pg-rw"}
	ksKey      = failoverv1.ObjectKey{Namespace: "flux-system", Name: "db"}
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	for _, gvk := range []schema.GroupVersionKind{KustomizationGVK, GitRepositoryGVK, clusterGVK, poolerGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

type fakeClients struct{ c client.Client }

func (f fakeClients) Client(string) (client.Client, error)    { return f.c, nil }
func (f fakeClients) Reachable(context.Context, string) error { return nil }

func newClient(funcs *interceptor.Funcs, objs ...client.Object) client.WithWatch {
	b := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func object(gvk schema.GroupVersionKind, ns, name string, content map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: content}
	if u.Object == nil {
		u.Object = map[string]any{}
	}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// liveCluster is a Cluster that Kustomization flux-system/db applies.
func liveCluster(annotations map[string]string) *unstructured.Unstructured {
	u := object(clusterGVK, "db", "pg", map[string]any{
		"spec": map[string]any{
			"instances": int64(3),
			"replica":   map[string]any{"primary": "pg-a", "source": "pg-a"},
		},
	})
	u.SetLabels(map[string]string{LabelKustomizationName: "db", LabelKustomizationNamespace: "flux-system"})
	u.SetAnnotations(annotations)
	u.SetGeneration(4)
	return u
}

func get(t *testing.T, c client.Client, ref failoverv1.ObjectRef) *unstructured.Unstructured {
	t.Helper()
	u, err := getObject(context.Background(), c, ref)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func wantStop(t *testing.T, err error, reason string, contains ...string) {
	t.Helper()
	se, ok := stop.As(err)
	if !ok {
		t.Fatalf("want stop %s, got %v", reason, err)
	}
	if se.Reason != reason || se.Wait {
		t.Fatalf("want stop %s, got %+v", reason, se)
	}
	for _, s := range contains {
		if !strings.Contains(se.Message, s) {
			t.Errorf("message %q lacks %q", se.Message, s)
		}
	}
}

func TestOwner(t *testing.T) {
	helm := object(clusterGVK, "db", "helm", nil)
	helm.SetLabels(map[string]string{LabelHelmName: "pg", "helm.toolkit.fluxcd.io/namespace": "db"})
	none := object(clusterGVK, "db", "none", nil)
	half := object(clusterGVK, "db", "half", nil)
	half.SetLabels(map[string]string{LabelKustomizationName: "db"})
	c := newClient(nil, liveCluster(nil), helm, none, half)
	f := New(fakeClients{c})
	ctx := context.Background()

	o, err := f.Owner(ctx, "a", clusterRef)
	if err != nil {
		t.Fatal(err)
	}
	if o.Helm || o.Kustomization == nil || *o.Kustomization != ksKey {
		t.Errorf("kustomize owner: got %+v", o)
	}
	ref := clusterRef
	ref.Name = "helm"
	if o, _ = f.Owner(ctx, "a", ref); !o.Helm || o.Kustomization != nil {
		t.Errorf("helm owner: got %+v", o)
	}
	for _, name := range []string{"none", "half"} {
		ref.Name = name
		if o, _ = f.Owner(ctx, "a", ref); o.Helm || o.Kustomization != nil {
			t.Errorf("%s: want no owner, got %+v", name, o)
		}
	}
	ref.Name = "missing"
	if _, err := f.Owner(ctx, "a", ref); !apierrors.IsNotFound(err) {
		t.Errorf("missing object: got %v", err)
	}
}

func TestHold(t *testing.T) {
	ctx := context.Background()
	t.Run("fresh", func(t *testing.T) {
		updates := 0
		funcs := &interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			o := &client.UpdateOptions{}
			o.ApplyOptions(opts)
			if o.FieldManager != "tack" {
				t.Errorf("field manager %q, want tack", o.FieldManager)
			}
			if obj.GetResourceVersion() == "" {
				t.Error("update without resourceVersion")
			}
			return c.Update(ctx, obj, opts...)
		}}
		c := newClient(funcs, liveCluster(map[string]string{"keep": "me"}))
		f := New(fakeClients{c})
		k, err := f.Hold(ctx, "a", clusterRef, "run-1", "")
		if err != nil {
			t.Fatal(err)
		}
		if k != ksKey {
			t.Errorf("kustomization %v, want %v", k, ksKey)
		}
		ann := get(t, c, clusterRef).GetAnnotations()
		if ann[AnnotationReconcile] != "disabled" || ann[AnnotationHeldBy] != "run-1" || ann["keep"] != "me" {
			t.Errorf("annotations %v", ann)
		}
		// Idempotent: no second write.
		if _, err := f.Hold(ctx, "a", clusterRef, "run-1", ""); err != nil {
			t.Fatal(err)
		}
		if updates != 1 {
			t.Errorf("%d updates, want 1", updates)
		}
	})
	t.Run("held by this run without the Flux annotation", func(t *testing.T) {
		c := newClient(nil, liveCluster(map[string]string{AnnotationHeldBy: "run-1"}))
		if _, err := New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", ""); err != nil {
			t.Fatal(err)
		}
		if v := get(t, c, clusterRef).GetAnnotations()[AnnotationReconcile]; v != "disabled" {
			t.Errorf("reconcile annotation %q", v)
		}
	})
	t.Run("other run", func(t *testing.T) {
		c := newClient(nil, liveCluster(map[string]string{AnnotationReconcile: "disabled", AnnotationHeldBy: "run-0"}))
		_, err := New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", "")
		wantStop(t, err, failoverv1.ReasonHeldByAnotherRun, "run-0")
		_, err = New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", "run-9")
		wantStop(t, err, failoverv1.ReasonHeldByAnotherRun, "run-0")
	})
	t.Run("take over", func(t *testing.T) {
		c := newClient(nil, liveCluster(map[string]string{AnnotationReconcile: "disabled", AnnotationHeldBy: "run-0"}))
		if _, err := New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", "run-0"); err != nil {
			t.Fatal(err)
		}
		if v := get(t, c, clusterRef).GetAnnotations()[AnnotationHeldBy]; v != "run-1" {
			t.Errorf("held by %q, want run-1", v)
		}
	})
	t.Run("held outside Tack", func(t *testing.T) {
		c := newClient(nil, liveCluster(map[string]string{AnnotationReconcile: "Disabled"}))
		_, err := New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", "")
		wantStop(t, err, failoverv1.ReasonHeldByAnotherRun, "held outside Tack")

		u := liveCluster(nil)
		u.SetLabels(map[string]string{LabelKustomizationName: "db", LabelKustomizationNamespace: "flux-system", AnnotationReconcile: "disabled"})
		c = newClient(nil, u)
		_, err = New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", "")
		wantStop(t, err, failoverv1.ReasonHeldByAnotherRun, "held outside Tack", "label")
	})
	t.Run("no owner", func(t *testing.T) {
		u := liveCluster(nil)
		u.SetLabels(nil)
		c := newClient(nil, u)
		if _, err := New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", ""); err == nil || !strings.Contains(err.Error(), "no Flux Kustomization") {
			t.Fatalf("got %v", err)
		}
		if ann := get(t, c, clusterRef).GetAnnotations(); len(ann) != 0 {
			t.Errorf("annotations written: %v", ann)
		}
	})
	t.Run("conflict once", func(t *testing.T) {
		n := 0
		funcs := &interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			n++
			if n == 1 {
				return apierrors.NewConflict(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, obj.GetName(), nil)
			}
			return c.Update(ctx, obj, opts...)
		}}
		c := newClient(funcs, liveCluster(nil))
		if _, err := New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", ""); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("%d updates, want 2", n)
		}
	})
	t.Run("conflict twice", func(t *testing.T) {
		funcs := &interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, obj.GetName(), nil)
		}}
		c := newClient(funcs, liveCluster(nil))
		if _, err := New(fakeClients{c}).Hold(ctx, "a", clusterRef, "run-1", ""); !apierrors.IsConflict(err) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRelease(t *testing.T) {
	ctx := context.Background()
	t.Run("held by this run", func(t *testing.T) {
		c := newClient(nil, liveCluster(map[string]string{AnnotationReconcile: "disabled", AnnotationHeldBy: "run-1", "keep": "me"}))
		gen, err := New(fakeClients{c}).Release(ctx, "a", clusterRef, "run-1")
		if err != nil {
			t.Fatal(err)
		}
		if gen != 4 {
			t.Errorf("generation %d, want 4", gen)
		}
		ann := get(t, c, clusterRef).GetAnnotations()
		if _, ok := ann[AnnotationReconcile]; ok {
			t.Errorf("reconcile annotation left: %v", ann)
		}
		if _, ok := ann[AnnotationHeldBy]; ok {
			t.Errorf("held-by annotation left: %v", ann)
		}
		if ann["keep"] != "me" {
			t.Errorf("other annotation lost: %v", ann)
		}
		// Idempotent.
		if gen, err = New(fakeClients{c}).Release(ctx, "a", clusterRef, "run-1"); err != nil || gen != 4 {
			t.Fatalf("second release: %d, %v", gen, err)
		}
	})
	t.Run("other run", func(t *testing.T) {
		c := newClient(nil, liveCluster(map[string]string{AnnotationReconcile: "disabled", AnnotationHeldBy: "run-2"}))
		_, err := New(fakeClients{c}).Release(ctx, "a", clusterRef, "run-1")
		wantStop(t, err, failoverv1.ReasonHeldByAnotherRun, "run-2")
		if v := get(t, c, clusterRef).GetAnnotations()[AnnotationReconcile]; v != "disabled" {
			t.Error("hold removed")
		}
	})
	t.Run("held outside Tack", func(t *testing.T) {
		c := newClient(nil, liveCluster(map[string]string{AnnotationReconcile: "disabled"}))
		_, err := New(fakeClients{c}).Release(ctx, "a", clusterRef, "run-1")
		wantStop(t, err, failoverv1.ReasonHeldByAnotherRun, "held outside Tack")
	})
}
