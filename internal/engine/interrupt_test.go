package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
)

// cancelOnRead cancels the run at the n-th check of a site, as a Ctrl-C
// during it does: the check then gets no answer.
type cancelOnRead struct {
	kube.Clients
	site   string
	n      int
	cancel context.CancelFunc
	calls  int
}

func (c *cancelOnRead) Reachable(ctx context.Context, site string) error {
	if site == c.site {
		c.calls++
		if c.calls == c.n {
			c.cancel()
			return &kube.UnreachableError{Err: context.Canceled}
		}
	}
	return c.Clients.Reachable(ctx, site)
}

// #35 row 34. Ctrl-C during the read before a site's steps is not a site
// that gave no answer: the run stops before the step, which keeps its
// state, and nothing at the site runs. It was recorded Deferred.
func TestInterruptDuringReadBeforeStepsKeepsStep(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	plan := f.plan("b", failoverv1.ModeSiteLoss)
	f.clients.SetDown("a", false)
	setPrimary(t, f, "a", "pg-b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := f.env("b")
	// The first check of a is the one before its first step; the second,
	// the read before its steps.
	env.Clients = &cancelOnRead{Clients: f.clients, site: "a", n: 2, cancel: cancel}
	err := env.Start(ctx, plan, engine.Options{})
	sf := f.ledger("b")
	if !errors.Is(err, engine.ErrStopped) || !strings.HasPrefix(sf.Status.Message, "step hold-a-cluster-pg-a: context canceled") {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	for _, s := range sf.Spec.Steps {
		if st := stepState(sf, s.ID); st != failoverv1.StepPending {
			t.Errorf("%s is %s, want Pending", s.ID, st)
		}
	}
	if holdsAt(f.flux, "a") != 0 {
		t.Error("site a was held")
	}
}
