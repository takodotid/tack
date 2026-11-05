package flux

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// RFC0005 Requirement 5: the hold, the release and the reconcile request
// write with field manager tack.
func TestWritesUseFieldManagerTack(t *testing.T) {
	ctx := context.Background()
	var writes []string
	funcs := &interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			o := &client.UpdateOptions{}
			o.ApplyOptions(opts)
			writes = append(writes, "update "+obj.GetObjectKind().GroupVersionKind().Kind+": "+o.FieldManager)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			o := &client.PatchOptions{}
			o.ApplyOptions(opts)
			writes = append(writes, "patch "+obj.GetObjectKind().GroupVersionKind().Kind+": "+o.FieldManager)
			return c.Patch(ctx, obj, p, opts...)
		},
	}
	ks := kustomization(ksKey, false)
	c := newClient(funcs, liveCluster(nil), ks)
	f := testFlux(c, nil)
	if _, err := f.Hold(ctx, "a", clusterRef, "run-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Release(ctx, "a", clusterRef, "run-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.request(ctx, c, ks); err != nil {
		t.Fatal(err)
	}
	want := "update Cluster: tack,update Cluster: tack,patch Kustomization: tack"
	if got := strings.Join(writes, ","); got != want {
		t.Errorf("writes %s, want %s", got, want)
	}
}
