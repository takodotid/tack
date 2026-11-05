package engine_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// failingIdentity fails the next reads of a site's kube-system namespace,
// which identifies its cluster, with errs in order.
type failingIdentity struct {
	client.WithWatch
	errs  []error
	reads int
}

func (c *failingIdentity) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Namespace); ok && key.Name == "kube-system" {
		c.reads++
		if len(c.errs) > 0 {
			err := c.errs[0]
			c.errs = c.errs[1:]
			return err
		}
	}
	return c.WithWatch.Get(ctx, key, obj, opts...)
}

// failIdentity makes the next reads of site's identity fail with errs.
func (f *fixture) failIdentity(site string, errs ...error) *failingIdentity {
	c := &failingIdentity{WithWatch: f.clients.Sites[site].(client.WithWatch), errs: errs}
	f.clients.SetSite(site, c)
	return c
}

var reset = &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

// #38 item 5. The read of a site's cluster identity, right after the
// site answered its check, gets no answer once. It is a blip: the read is
// repeated, and the run goes on. v1.2.1 stopped with StepFailed.
func TestIdentityReadRetried(t *testing.T) {
	f := newFixture(t)
	sf := f.plan("b", failoverv1.ModePlanned)
	c := f.failIdentity("a", reset)
	sf, err := f.run("b", sf, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if c.reads != 2 {
		t.Errorf("%d reads of site a's identity, want 2", c.reads)
	}
}

// #38 item 5. In a site loss, the lost site answers its check, then gives
// no answer to any read of its identity: its steps are deferred, as when
// it gives no answer to the read before its steps, and the run promotes b.
func TestIdentityReadWithoutAnswerDefersSteps(t *testing.T) {
	f := newFixture(t)
	// The operator fenced a out of band before the run (docs/runbooks/site-loss.md).
	setPrimary(t, f, "a", "pg-b")
	sf := f.plan("b", failoverv1.ModeSiteLoss)
	c := f.failIdentity("a", reset, reset, reset)
	sf, err := f.run("b", sf, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if c.reads != 3 {
		t.Errorf("%d reads of site a's identity, want 3", c.reads)
	}
	for _, id := range []string{"hold-a-cluster-pg-a", "fence-a"} {
		if st := stepState(sf, id); st != failoverv1.StepDeferred {
			t.Errorf("%s is %s, want Deferred", id, st)
		}
	}
	if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "no answer to 3 reads") {
		t.Errorf("hold-a-cluster-pg-a: message %q", msg)
	}
	if st := stepState(sf, "promote-b"); st != failoverv1.StepSucceeded {
		t.Errorf("promote-b is %s", st)
	}
}

// A read of the identity that the site refuses is no blip: the run stops
// with StepFailed before the step, at the first read.
func TestIdentityReadRefusedStops(t *testing.T) {
	f := newFixture(t)
	sf := f.plan("b", failoverv1.ModePlanned)
	c := f.failIdentity("a", apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "kube-system", errors.New("no")))
	sf, err := f.run("b", sf, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed {
		t.Fatalf("run: %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if c.reads != 1 {
		t.Errorf("%d reads of site a's identity, want 1", c.reads)
	}
	if st := stepState(sf, "hold-a-cluster-pg-a"); st != failoverv1.StepPending {
		t.Errorf("hold-a-cluster-pg-a is %s, want Pending", st)
	}
}
