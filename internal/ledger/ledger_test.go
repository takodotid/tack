package ledger

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/kube"
)

// TestNotServed checks the error from a cluster whose CRD does not serve
// the ledger's version, as the API server reports it.
func TestNotServed(t *testing.T) {
	noMatch := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: failoverv1.GroupVersion.Group, Kind: "SiteFailover"},
		SearchedVersions: []string{failoverv1.GroupVersion.Version},
	}
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(kube.Scheme).Build(), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return noMatch
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return noMatch
		},
	})
	s := &Store{Client: c, Namespace: "tack-system"}
	_, err := s.List(context.Background())
	if !meta.IsNoMatchError(err) || !strings.Contains(err.Error(), "does not serve SiteFailover failover.tako.id/v1: install or upgrade Tack's CRD") {
		t.Errorf("List: got %v", err)
	}
	if _, err := s.Get(context.Background(), "pg-main-20261027-101500"); !meta.IsNoMatchError(err) || !strings.Contains(err.Error(), "install or upgrade Tack's CRD") {
		t.Errorf("Get: got %v", err)
	}
}

// TestCreateRefusesPrunedSpec checks a cluster whose CRD is older than
// Tack: the API server drops the spec fields it does not know, without an
// error. Create must refuse before the ledger exists.
func TestCreateRefusesPrunedSpec(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if sf, ok := o.(*failoverv1.SiteFailover); ok {
				sf.Spec.TakeOver = ""
			}
			return cl.Create(ctx, o, opts...)
		},
	})
	s := &Store{Client: c, Namespace: "tack-system"}
	sf := &failoverv1.SiteFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "run-2"},
		Spec:       failoverv1.SiteFailoverSpec{Mode: failoverv1.ModeSiteLoss, From: "a", To: "b", TakeOver: "run-1"},
	}
	err := s.Create(context.Background(), sf)
	if err == nil || !strings.Contains(err.Error(), "apply the CRD of this Tack version") {
		t.Fatalf("Create: got %v", err)
	}
	var l failoverv1.SiteFailoverList
	if err := base.List(context.Background(), &l); err != nil || len(l.Items) != 0 {
		t.Errorf("a ledger was created: %d, %v", len(l.Items), err)
	}

	ok := &Store{Client: base, Namespace: "tack-system"}
	if err := ok.Create(context.Background(), sf.DeepCopy()); err != nil {
		t.Fatalf("Create on a current CRD: %v", err)
	}
}
