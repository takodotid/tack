package strait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

func obj(gen, observed int64, reason string, members ...any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"generation": gen},
		"status": map[string]any{
			"observedGeneration": observed,
			"route":              map[string]any{"members": members, "reason": reason},
		},
	}
}

func TestRouted(t *testing.T) {
	tests := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"routed", obj(2, 2, ReasonActive, "pg-b"), true},
		{"old member", obj(2, 2, ReasonActive, "pg-a"), false},
		{"nowhere", obj(2, 2, "Transitioning"), false},
		{"two members", obj(2, 2, ReasonActive, "pg-a", "pg-b"), false},
		{"stale status", obj(3, 2, ReasonActive, "pg-b"), false},
		{"no status", map[string]any{"metadata": map[string]any{"generation": int64(1)}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := Routed(tt.obj, "pg-b")
			if got != tt.want {
				t.Fatalf("Routed = %v (%s), want %v", got, why, tt.want)
			}
		})
	}
}

// sites returns the same client for every site.
type sites struct{ c client.Client }

func (s sites) Client(string) (client.Client, error)    { return s.c, nil }
func (s sites) Reachable(context.Context, string) error { return nil }

// reader returns a Reader whose Get answers with the objects in turn, the
// last one from then on, and counts the reads.
func reader(reads *int, answers ...func(*unstructured.Unstructured) error) *Reader {
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, o client.Object, _ ...client.GetOption) error {
			i := min(*reads, len(answers)-1)
			*reads++
			return answers[i](o.(*unstructured.Unstructured))
		},
	}).Build()
	return &Reader{Clients: sites{c}, Poll: time.Millisecond, Now: time.Now}
}

func status(o map[string]any) func(*unstructured.Unstructured) error {
	return func(u *unstructured.Unstructured) error {
		u.Object = o
		return nil
	}
}

func step(timeout time.Duration) *failoverv1.RoutingStep {
	return &failoverv1.RoutingStep{
		Service:  failoverv1.ObjectKey{Namespace: "db", Name: "postgres"},
		Expected: "pg-b",
		Timeout:  metav1.Duration{Duration: timeout},
	}
}

func TestWaitPollsUntilRouted(t *testing.T) {
	var reads int
	r := reader(&reads,
		status(obj(2, 1, ReasonActive, "pg-a")),
		status(obj(2, 2, "Transitioning")),
		status(obj(2, 2, ReasonActive, "pg-b")),
	)
	if _, err := r.Wait(context.Background(), "a", step(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if reads != 3 {
		t.Fatalf("reads = %d, want 3", reads)
	}
}

func TestWaitNoStrait(t *testing.T) {
	var reads int
	r := reader(&reads, func(*unstructured.Unstructured) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "strait.tako.id", Kind: Kind}}
	})
	_, err := r.Wait(context.Background(), "a", step(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "is Strait v1.0.0 or later installed?") {
		t.Fatalf("err = %v, want Strait not installed", err)
	}
}

func TestWaitTimesOut(t *testing.T) {
	var reads int
	r := reader(&reads, status(obj(2, 2, "DeclarationsDisagree")))
	_, err := r.Wait(context.Background(), "a", step(20*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "DeclarationsDisagree") {
		t.Fatalf("err = %v, want a timeout naming the route", err)
	}
}

// getOnly returns a client whose Get answers with the objects in turn, the last one from then on, and that fails
// the test on any other call to the API.
func getOnly(t *testing.T, answers ...func(*unstructured.Unstructured) error) client.Client {
	t.Helper()
	var reads int
	fail := func(call string) error {
		t.Errorf("the Strait reader called %s; it may only get a SiteService ([RFC#0008] Requirement 1)", call)
		return errors.New(call + " is not allowed")
	}
	return interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, o client.Object, _ ...client.GetOption) error {
			i := min(reads, len(answers)-1)
			reads++
			return answers[i](o.(*unstructured.Unstructured))
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return fail("List")
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return fail("Create")
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return fail("Delete")
		},
		DeleteAllOf: func(context.Context, client.WithWatch, client.Object, ...client.DeleteAllOfOption) error {
			return fail("DeleteAllOf")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return fail("Update")
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return fail("Patch")
		},
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return fail("Apply")
		},
		Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return nil, fail("Watch")
		},
		SubResourceGet: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceGetOption) error {
			return fail("a subresource's Get")
		},
		SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
			return fail("a subresource's Create")
		},
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return fail("a subresource's Update")
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return fail("a subresource's Patch")
		},
		SubResourceApply: func(context.Context, client.Client, string, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
			return fail("a subresource's Apply")
		},
	})
}

// TestReaderOnlyGets: [RFC#0001] Requirement 3 and [RFC#0008] Requirement 1. The reader only gets the SiteService:
// while it polls until routed, when Strait is not installed, and until it times out.
func TestReaderOnlyGets(t *testing.T) {
	noStrait := func(*unstructured.Unstructured) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "strait.tako.id", Kind: Kind}}
	}
	for name, tt := range map[string]struct {
		answers []func(*unstructured.Unstructured) error
		timeout time.Duration
		ok      bool
	}{
		"routed":    {[]func(*unstructured.Unstructured) error{status(obj(2, 1, ReasonActive, "pg-a")), status(obj(2, 2, ReasonActive, "pg-b"))}, time.Minute, true},
		"no Strait": {[]func(*unstructured.Unstructured) error{noStrait}, time.Minute, false},
		"timeout":   {[]func(*unstructured.Unstructured) error{status(obj(2, 2, "Transitioning"))}, 20 * time.Millisecond, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := &Reader{Clients: sites{getOnly(t, tt.answers...)}, Poll: time.Millisecond, Now: time.Now}
			_, err := r.Wait(context.Background(), "a", step(tt.timeout))
			if (err == nil) != tt.ok {
				t.Fatalf("Wait: %v, want success %v", err, tt.ok)
			}
		})
	}
}
