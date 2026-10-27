package flux

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/takodotid/tack/api/v1alpha1"
)

const (
	oldRev = "main@sha1:1111111111111111111111111111111111111111"
	newRev = "main@sha1:2222222222222222222222222222222222222222"
)

func readyCond(status, reason, msg string) []any {
	return []any{map[string]any{"type": "Ready", "status": status, "reason": reason, "message": msg}}
}

func kustomization(key v1alpha1.ObjectKey, suspend bool) *unstructured.Unstructured {
	u := object(KustomizationGVK, key.Namespace, key.Name, map[string]any{
		"spec": map[string]any{
			"suspend":   suspend,
			"path":      "./clusters/a",
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"},
		},
		"status": map[string]any{
			"observedGeneration":     int64(1),
			"lastHandledReconcileAt": "old",
			"lastAppliedRevision":    oldRev,
			"conditions":             readyCond("True", ReasonSucceeded, "Applied revision: "+oldRev),
		},
	})
	u.SetGeneration(1)
	return u
}

func gitRepository() *unstructured.Unstructured {
	return object(GitRepositoryGVK, "flux-system", "flux-system", map[string]any{
		"spec": map[string]any{"url": "ssh://git.example.org/repo", "ref": map[string]any{"branch": "main"}},
		"status": map[string]any{
			"artifact":   map[string]any{"revision": oldRev},
			"conditions": readyCond("True", "Succeeded", "stored artifact"),
		},
	})
}

// fluxSim answers reconcile requests the way the controllers would, by
// calling react with the object and the token right after the request.
type fluxSim struct {
	mu       sync.Mutex
	requests []string
	react    func(u *unstructured.Unstructured, tok string)
}

func (s *fluxSim) funcs() *interceptor.Funcs {
	return &interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return nil
		}
		tok := u.GetAnnotations()[AnnotationRequestedAt]
		s.mu.Lock()
		s.requests = append(s.requests, u.GetKind()+" "+u.GetNamespace()+"/"+u.GetName())
		s.mu.Unlock()
		if s.react == nil {
			return nil
		}
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(u.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(u), cur); err != nil {
			return err
		}
		s.react(cur, tok)
		return c.Update(ctx, cur)
	}}
}

func setStatus(u *unstructured.Unstructured, value any, fields ...string) {
	if err := unstructured.SetNestedField(u.Object, value, append([]string{"status"}, fields...)...); err != nil {
		panic(err)
	}
}

// succeed makes the source fetch newRev and each Kustomization apply what
// apply returns for it.
func succeed(apply func(name string) string) func(u *unstructured.Unstructured, tok string) {
	return func(u *unstructured.Unstructured, tok string) {
		setStatus(u, tok, "lastHandledReconcileAt")
		switch u.GetKind() {
		case "GitRepository":
			setStatus(u, newRev, "artifact", "revision")
		case "Kustomization":
			rev := apply(u.GetName())
			setStatus(u, rev, "lastAppliedRevision")
			setStatus(u, u.GetGeneration(), "observedGeneration")
			setStatus(u, readyCond("True", ReasonSucceeded, "Applied revision: "+rev), "conditions")
		}
	}
}

func testFlux(c client.Client, out *bytes.Buffer) *Flux {
	f := New(fakeClients{c})
	f.Poll = time.Millisecond
	if out != nil {
		f.Out = out
	}
	return f
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	always := func(rev string) func(string) string { return func(string) string { return rev } }

	t.Run("success", func(t *testing.T) {
		sim := &fluxSim{react: succeed(always(newRev))}
		c := newClient(sim.funcs(), kustomization(ksKey, false), gitRepository())
		rev, err := testFlux(c, nil).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey, ksKey}, "", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if rev != newRev {
			t.Errorf("revision %q, want %q", rev, newRev)
		}
		want := []string{"GitRepository flux-system/flux-system", "Kustomization flux-system/db"}
		if strings.Join(sim.requests, ",") != strings.Join(want, ",") {
			t.Errorf("requests %v, want %v", sim.requests, want)
		}
	})
	t.Run("expected revision", func(t *testing.T) {
		sim := &fluxSim{react: succeed(always(newRev))}
		c := newClient(sim.funcs(), kustomization(ksKey, false), gitRepository())
		rev, err := testFlux(c, nil).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "2222222222222222222222222222222222222222", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if rev != newRev {
			t.Errorf("revision %q, want %q", rev, newRev)
		}
	})
	t.Run("suspended", func(t *testing.T) {
		sim := &fluxSim{react: succeed(always(newRev))}
		c := newClient(sim.funcs(), kustomization(ksKey, true), gitRepository())
		var out bytes.Buffer
		rev, err := testFlux(c, &out).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if rev != "" || len(sim.requests) != 0 {
			t.Errorf("revision %q, requests %v", rev, sim.requests)
		}
		if !strings.Contains(out.String(), "suspended") {
			t.Errorf("no note: %q", out.String())
		}
	})
	t.Run("wrong revision", func(t *testing.T) {
		sim := &fluxSim{react: succeed(always(newRev))}
		c := newClient(sim.funcs(), kustomization(ksKey, false), gitRepository())
		_, err := testFlux(c, nil).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "3333333333333333333333333333333333333333", 30*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "applied "+newRev) ||
			!strings.Contains(err.Error(), ReasonSucceeded) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("failed run", func(t *testing.T) {
		sim := &fluxSim{react: func(u *unstructured.Unstructured, tok string) {
			setStatus(u, tok, "lastHandledReconcileAt")
			if u.GetKind() == "Kustomization" {
				setStatus(u, readyCond("False", "ReconciliationFailed", "apply failed: boom"), "conditions")
			}
		}}
		c := newClient(sim.funcs(), kustomization(ksKey, false), gitRepository())
		_, err := testFlux(c, nil).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "", 30*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "ReconciliationFailed: apply failed: boom") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("stale Ready after a run that returned early", func(t *testing.T) {
		sim := &fluxSim{react: func(u *unstructured.Unstructured, tok string) {
			setStatus(u, tok, "lastHandledReconcileAt")
			if u.GetKind() == "Kustomization" {
				u.SetGeneration(2)
			}
		}}
		c := newClient(sim.funcs(), kustomization(ksKey, false), gitRepository())
		_, err := testFlux(c, nil).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "", 30*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "observed generation 1, want 2") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		c := newClient((&fluxSim{}).funcs(), kustomization(ksKey, false), gitRepository())
		var out bytes.Buffer
		_, err := testFlux(c, &out).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "", 30*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "has not handled the reconcile request") {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(out.String(), "GitRepository flux-system/flux-system at site a did not handle") {
			t.Errorf("no source note: %q", out.String())
		}
	})
	t.Run("source fails to fetch", func(t *testing.T) {
		sim := &fluxSim{react: func(u *unstructured.Unstructured, tok string) {
			if u.GetKind() == "GitRepository" {
				setStatus(u, tok, "lastHandledReconcileAt")
				setStatus(u, readyCond("False", "GitOperationFailed", "dial tcp: no route to host"), "conditions")
				return
			}
			succeed(always(oldRev))(u, tok)
		}}
		c := newClient(sim.funcs(), kustomization(ksKey, false), gitRepository())
		var out bytes.Buffer
		rev, err := testFlux(c, &out).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if rev != oldRev {
			t.Errorf("revision %q, want %q", rev, oldRev)
		}
		if !strings.Contains(out.String(), "did not fetch (GitOperationFailed") {
			t.Errorf("no note: %q", out.String())
		}
	})
	t.Run("Kustomizations disagree", func(t *testing.T) {
		other := v1alpha1.ObjectKey{Namespace: "flux-system", Name: "apps"}
		sim := &fluxSim{react: succeed(func(name string) string {
			if name == "apps" {
				return oldRev
			}
			return newRev
		})}
		c := newClient(sim.funcs(), kustomization(ksKey, false), kustomization(other, false), gitRepository())
		_, err := testFlux(c, nil).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey, other}, "", time.Second)
		wantStop(t, err, v1alpha1.ReasonGitChanged, oldRev, newRev)
	})
	t.Run("missing Kustomization", func(t *testing.T) {
		c := newClient(nil, gitRepository())
		if _, err := testFlux(c, nil).Reconcile(ctx, "a", []v1alpha1.ObjectKey{ksKey}, "", time.Second); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestRequestToken(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 2, 3, 4, time.UTC)
	u := kustomization(ksKey, false)
	u.SetAnnotations(map[string]string{AnnotationRequestedAt: now.Format(time.RFC3339Nano)})
	c := newClient(nil, u)
	f := testFlux(c, nil)
	f.Now = func() time.Time { return now }
	tok, err := f.request(context.Background(), c, u)
	if err != nil {
		t.Fatal(err)
	}
	if tok == now.Format(time.RFC3339Nano) {
		t.Errorf("token %q repeats the previous request", tok)
	}
	got, _ := getKind(context.Background(), c, KustomizationGVK, ksKey)
	if got.GetAnnotations()[AnnotationRequestedAt] != tok {
		t.Errorf("annotation %q, want %q", got.GetAnnotations()[AnnotationRequestedAt], tok)
	}
}
