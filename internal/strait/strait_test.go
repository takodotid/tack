package strait

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
