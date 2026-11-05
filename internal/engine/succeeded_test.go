package engine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// recording keeps, for each ledger write, the run's state and the steps
// that are Running.
type recording struct {
	engine.Ledger
	mu     sync.Mutex
	writes []string
	// failRunning fails the first write that records the run Running: the
	// process dies there.
	failRunning bool
}

func (l *recording) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	var running []string
	for _, st := range sf.Status.Steps {
		if st.State == failoverv1.StepRunning {
			running = append(running, st.ID)
		}
	}
	l.mu.Lock()
	l.writes = append(l.writes, fmt.Sprintf("%s %v", sf.Status.State, running))
	fail := l.failRunning && sf.Status.State == failoverv1.RunRunning
	l.mu.Unlock()
	if fail {
		return errors.New("the process died")
	}
	return l.Ledger.UpdateStatus(ctx, sf)
}

// succeededWithADown runs a site loss toward b with site a down: the run
// succeeds with a's steps deferred.
func succeededWithADown(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded || stepState(sf, "fence-a") != failoverv1.StepDeferred {
		t.Fatalf("run: %v %s %s, fence-a %s", err, sf.Status.State, sf.Status.Message, stepState(sf, "fence-a"))
	}
	return f
}

// #35 row 31. A resume of a run that succeeded with deferred steps, while
// their site is still down, never records the run Running: a process that
// dies during it must not leave a finished run Running, which blocks
// every new run of the workload.
func TestResumeSucceededRunKeepsItsState(t *testing.T) {
	f := succeededWithADown(t)
	env := f.env("b")
	l := &recording{Ledger: env.Ledger}
	env.Ledger = l
	if err := env.Resume(context.Background(), f.ledger("b"), engine.Options{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for _, w := range l.writes {
		if w != "Succeeded []" {
			t.Errorf("a write records %s; want the run Succeeded, as nothing ran (writes %q)", w, l.writes)
		}
	}
	if sf := f.ledger("b"); sf.Status.State != failoverv1.RunSucceeded || stepState(sf, "fence-a") != failoverv1.StepDeferred {
		t.Errorf("%s, fence-a %s", sf.Status.State, stepState(sf, "fence-a"))
	}
}

// #35 row 31. Once site a is back, the run is recorded Running with its
// first deferred step, in one write, and not before. A process that dies
// at that write leaves the run Succeeded.
func TestResumeSucceededRunRunsWithItsFirstStep(t *testing.T) {
	f := succeededWithADown(t)
	f.clients.SetDown("a", false)
	setPrimary(t, f, "a", "pg-b")
	env := f.env("b")
	l := &recording{Ledger: env.Ledger, failRunning: true}
	env.Ledger = l
	if err := env.Resume(context.Background(), f.ledger("b"), engine.Options{}); err == nil {
		t.Fatal("the resume went on after its process died")
	}
	if len(l.writes) != 1 || l.writes[0] != "Running [hold-a-cluster-pg-a]" {
		t.Errorf("writes %q, want the first one to record the run Running with hold-a-cluster-pg-a", l.writes)
	}
	if sf := f.ledger("b"); sf.Status.State != failoverv1.RunSucceeded {
		t.Errorf("the run is %s after the process died before its first step", sf.Status.State)
	}
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded || stepState(sf, "fence-a") != failoverv1.StepSucceeded {
		t.Fatalf("resume: %v %s %s, fence-a %s", err, sf.Status.State, sf.Status.Message, stepState(sf, "fence-a"))
	}
	// RFC0003 Requirement 22: the write that records the first step
	// Running records the resume in the history; the process that died
	// before it recorded nothing.
	if got, want := entries(sf), []string{"Running", "Succeeded", "Running --fence=a", "Succeeded --fence=a"}; !slices.Equal(got, want) {
		t.Errorf("history %v, want %v", got, want)
	}
}
