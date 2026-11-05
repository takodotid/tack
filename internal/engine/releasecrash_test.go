package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// diesAfter fails the ledger write that records step Succeeded: the
// process died after the step acted, before the ledger recorded it.
type diesAfter struct {
	engine.Ledger
	step string
}

func (l *diesAfter) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	for _, st := range sf.Status.Steps {
		if st.ID == l.step && st.State == failoverv1.StepSucceeded {
			return errors.New("the process died")
		}
	}
	return l.Ledger.UpdateStatus(ctx, sf)
}

// heldGeneration returns the generation the ledger recorded for a held
// object.
func heldGeneration(t *testing.T, sf *failoverv1.SiteFailover, site, name string) int64 {
	t.Helper()
	for _, h := range sf.Status.Held {
		if h.Site == site && h.Object.Name == name {
			return h.Generation
		}
	}
	t.Fatalf("site %s: %s is not in the ledger's held objects", site, name)
	return 0
}

// releaseThenDie runs a planned run toward b whose process dies after
// release-a-cluster-pg-a removed the hold, before the ledger recorded the
// release. Flux then applies a change to Cluster pg-a, which no run holds.
func releaseThenDie(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	env := f.env("b")
	env.Ledger = &diesAfter{Ledger: env.Ledger, step: "release-a-cluster-pg-a"}
	if err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{}); err == nil {
		t.Fatal("the run went on after its process died")
	}
	if st := stepState(f.ledger("b"), "release-a-cluster-pg-a"); st != failoverv1.StepRunning {
		t.Fatalf("release-a-cluster-pg-a is %s, want Running", st)
	}
	if by := f.flux.Held["a/Cluster/pg-a"]; by != "" {
		t.Fatalf("Cluster pg-a is still held by %s", by)
	}
	f.flux.Generations = map[string]int64{"a/Cluster/pg-a": 2}
	return f
}

// #35 row 36 (RFC0005 Requirement 9). The resume finds no hold on the
// object. It keeps the generation the ledger recorded while the run held
// the object, so the check after release stops on the change Flux made in
// between, instead of taking it as the baseline.
func TestReleaseCrashKeepsGenerationBeforeRelease(t *testing.T) {
	f := releaseThenDie(t)
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFluxChanged ||
		!strings.HasPrefix(sf.Status.Message, "step verify-noop-a: ") {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if g := heldGeneration(t, sf, "a", "pg-a"); g != 1 {
		t.Errorf("the ledger records generation %d for pg-a, want 1, the one before the hold was removed", g)
	}
	if msg := stepMessage(sf, "release-a-cluster-pg-a"); !strings.Contains(msg, "the hold was already gone: generation 1 when the run removed it, 2 now") {
		t.Errorf("release-a-cluster-pg-a message %q", msg)
	}
}

// A ledger that a v1.2.x process left at the same point has no generation
// for the object: the resume records the one it reads, as v1.2.x did.
func TestReleaseCrashWithoutRecordedGeneration(t *testing.T) {
	f := releaseThenDie(t)
	sf := f.ledger("b")
	for i := range sf.Status.Held {
		sf.Status.Held[i].Generation = 0
	}
	if err := f.env("b").Ledger.UpdateStatus(context.Background(), sf); err != nil {
		t.Fatal(err)
	}
	sf, err := f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if g := heldGeneration(t, sf, "a", "pg-a"); g != 2 {
		t.Errorf("generation %d, want 2", g)
	}
}
