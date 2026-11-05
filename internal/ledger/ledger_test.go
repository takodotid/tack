package ledger

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return noMatch
		},
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
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
	// RFC0003 Requirement 16: the create and the status write say the same.
	sf := &failoverv1.SiteFailover{ObjectMeta: metav1.ObjectMeta{Name: "pg-main-20261027-101500", ResourceVersion: "1"}}
	if err := s.Create(context.Background(), sf.DeepCopy()); !meta.IsNoMatchError(err) || !strings.Contains(err.Error(), "install or upgrade Tack's CRD") {
		t.Errorf("Create: got %v", err)
	}
	if err := s.UpdateStatus(context.Background(), sf.DeepCopy()); !meta.IsNoMatchError(err) || !strings.Contains(err.Error(), "install or upgrade Tack's CRD") {
		t.Errorf("UpdateStatus: got %v", err)
	}
}

// RFC0003 Requirement 7: creating a ledger never replaces one with the
// same name.
func TestCreateRefusesTakenName(t *testing.T) {
	ctx := context.Background()
	s := &Store{Client: fake.NewClientBuilder().WithScheme(kube.Scheme).Build(), Namespace: "tack-system"}
	first := &failoverv1.SiteFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1"},
		Spec:       failoverv1.SiteFailoverSpec{Mode: failoverv1.ModePlanned, From: "a", To: "b"},
	}
	if err := s.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &failoverv1.SiteFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1"},
		Spec:       failoverv1.SiteFailoverSpec{Mode: failoverv1.ModeSiteLoss, From: "b", To: "a"},
	}
	if err := s.Create(ctx, second); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Create on a taken name: got %v, want AlreadyExists", err)
	}
	got, err := s.Get(ctx, "run-1")
	if err != nil || got.Spec.Mode != failoverv1.ModePlanned || got.Spec.To != "b" {
		t.Errorf("the ledger was replaced: %v, %s to %s", err, got.Spec.Mode, got.Spec.To)
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

// fieldManagers returns a fake client that records the field manager of
// each create and status update, as "create Kind: manager", with
// "dry-run" for a dry run.
func fieldManagers(writes *[]string) client.Client {
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(kube.Scheme).WithStatusSubresource(&failoverv1.SiteFailover{}).Build(), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			o := &client.CreateOptions{}
			o.ApplyOptions(opts)
			kind := strings.TrimPrefix(fmt.Sprintf("%T", obj), "*v1.")
			if len(o.DryRun) > 0 {
				kind += " dry-run"
			}
			*writes = append(*writes, "create "+kind+": "+o.FieldManager)
			return c.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			o := &client.SubResourceUpdateOptions{}
			o.ApplyOptions(opts)
			*writes = append(*writes, "update "+sub+": "+o.FieldManager)
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
}

// PR #43's finding: ledger writes named no field manager, so the API
// server took the client's user agent, the binary's name. Each write now
// names tack ([RFC#0003] Requirement 25).
func TestLedgerWritesUseFieldManagerTack(t *testing.T) {
	ctx := context.Background()
	var writes []string
	s := &Store{Client: fieldManagers(&writes), Namespace: "tack-system"}
	sf := &failoverv1.SiteFailover{ObjectMeta: metav1.ObjectMeta{Name: "pg-main-20261027-101500"}}
	if err := s.Create(ctx, sf); err != nil {
		t.Fatal(err)
	}
	sf.Status.State = failoverv1.RunRunning
	if err := s.UpdateStatus(ctx, sf); err != nil {
		t.Fatal(err)
	}
	want := "create SiteFailover dry-run: tack,create SiteFailover: tack,update status: tack"
	if got := strings.Join(writes, ","); got != want {
		t.Errorf("writes %s, want %s", got, want)
	}
}

// RFC0003 Requirement 25: the ledger's Events name tack too.
func TestEventsUseFieldManagerTack(t *testing.T) {
	var writes []string
	s := &Store{Client: fieldManagers(&writes), Namespace: "tack-system"}
	sf := &failoverv1.SiteFailover{ObjectMeta: metav1.ObjectMeta{Name: "pg-main-20261027-101500", Namespace: "tack-system"}}
	if err := s.Event(context.Background(), sf, false, "RunRunning", "started", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(writes, ","); got != "create Event: tack" {
		t.Errorf("writes %s, want create Event: tack", got)
	}
}
