package testenv

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// #38 item 4. The fake client kept a SiteFailover's status on create, but
// the API server drops it: its status subresource ignores status on
// create. Tests that needed the real behaviour modelled it themselves. The
// fake drops it, in the store and in the object the create returns, and a
// status write after the create keeps it.
func TestCreateDropsStatus(t *testing.T) {
	ctx := context.Background()
	c := NewClient()
	now := metav1.Now()
	sf := &failoverv1.SiteFailover{ObjectMeta: metav1.ObjectMeta{Namespace: "tack-system", Name: "run-1"},
		Status: failoverv1.SiteFailoverStatus{State: failoverv1.RunRunning, StartedAt: &now}}
	if err := c.Create(ctx, sf); err != nil {
		t.Fatal(err)
	}
	if sf.Status.State != "" || sf.Status.StartedAt != nil {
		t.Errorf("the create returned status %+v, want none", sf.Status)
	}
	got := &failoverv1.SiteFailover{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sf), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State != "" {
		t.Errorf("the stored ledger has state %q, want none", got.Status.State)
	}
	got.Status.State = failoverv1.RunRunning
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sf), got); err != nil || got.Status.State != failoverv1.RunRunning {
		t.Errorf("after a status write: state %q, %v", got.Status.State, err)
	}
}
