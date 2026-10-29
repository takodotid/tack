package ledger

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
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
