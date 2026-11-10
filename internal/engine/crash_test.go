package engine_test

import (
	"context"
	"errors"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// diesAfterCreate is a ledger whose process dies right after it created
// the ledger: the first status write never happens.
type diesAfterCreate struct {
	engine.Ledger
	died bool
}

func (l *diesAfterCreate) Create(ctx context.Context, sf *failoverv1.SiteFailover) error {
	return l.Ledger.Create(ctx, sf)
}

func (l *diesAfterCreate) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	if !l.died {
		l.died = true
		return errors.New("the process died")
	}
	return l.Ledger.UpdateStatus(ctx, sf)
}

// interruptedAtCreate is a ledger whose run is interrupted (Ctrl-C) right
// after it created the ledger. Like a real client, it refuses a write on a
// cancelled context.
type interruptedAtCreate struct {
	engine.Ledger
	cancel context.CancelFunc
}

func (l *interruptedAtCreate) Create(ctx context.Context, sf *failoverv1.SiteFailover) error {
	err := l.Ledger.Create(ctx, sf)
	l.cancel()
	return err
}

func (l *interruptedAtCreate) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.Ledger.UpdateStatus(ctx, sf)
}

// Issue #28. A site-loss run dies between the ledger's create and its first
// status write. The API server drops status on create, so the ledger has no
// state, no start and no Rebuild follow-up. A resume must not promote b
// and end without the follow-up.
func TestResumeRefusesLedgerThatNeverStarted(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	env := f.env("b")
	env.Ledger = &diesAfterCreate{Ledger: env.Ledger}
	if err := env.Start(context.Background(), f.plan("b", failoverv1.ModeSiteLoss), engine.Options{}); err == nil {
		t.Fatal("the run went on after its process died")
	}
	sf := f.ledger("b")
	err := f.env("b").Resume(context.Background(), sf, engine.Options{})
	if err == nil {
		t.Error("the resume of a ledger that never recorded its start was not refused")
	}
	if field(f.cluster("b"), "status", "instancesReportedState", "pg-b-1", "isPrimary") == true {
		sf = f.ledger("b")
		t.Fatalf("site b was promoted; the run is %s with follow-ups %v", sf.Status.State, sf.Status.FollowUps)
	}
}

// Issue #28. A site-loss run is interrupted right after the ledger's
// create. The first status write must still land: the ledger then has its
// start and its Rebuild follow-up.
func TestInterruptAfterCreateKeepsFollowUps(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	ctx, cancel := context.WithCancel(context.Background())
	env := f.env("b")
	env.Ledger = &interruptedAtCreate{Ledger: env.Ledger, cancel: cancel}
	_ = env.Start(ctx, f.plan("b", failoverv1.ModeSiteLoss), engine.Options{})
	sf := f.ledger("b")
	if sf.Status.StartedAt == nil || len(sf.Status.FollowUps) != 1 || sf.Status.FollowUps[0].Action != engine.FollowUpRebuild {
		t.Fatalf("state %q, startedAt %v, follow-ups %v", sf.Status.State, sf.Status.StartedAt, sf.Status.FollowUps)
	}
	sf, err := f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded || len(sf.Status.FollowUps) != 1 {
		t.Fatalf("resume: %v %s, follow-ups %v", err, sf.Status.State, sf.Status.FollowUps)
	}
}
