package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/version"
)

// setVersion makes this tack's version v until the test ends.
func setVersion(t *testing.T, v string) {
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// #35 row 26. The ledger did not record which tack wrote it, and a status
// write replaces the whole status, so an older tack that resumed a ledger
// dropped the status fields a newer one added. Every write now records
// status.writtenBy, and an older release refuses the ledger before it
// asks the operator, changing nothing ([RFC#0003] Requirement 24).
func TestResumeRefusesLedgerOfNewerTack(t *testing.T) {
	for _, tt := range []struct {
		tack, writtenBy string
		refused         bool
	}{
		{tack: "v1.3.0", writtenBy: "v1.3.1", refused: true},
		{tack: "v1.3.0", writtenBy: "v1.4.0", refused: true},
		{tack: "v1.3.0", writtenBy: "v1.3.0-2-g1fda25c", refused: true},
		{tack: "v1.3.0", writtenBy: "v1.3.0"},
		{tack: "v1.3.1", writtenBy: "v1.3.0"},
		{tack: "v1.3.0", writtenBy: ""},
		{tack: "dev", writtenBy: "v1.4.0"},
		{tack: "v1.3.0", writtenBy: "dev"},
	} {
		t.Run(tt.tack+" over "+tt.writtenBy, func(t *testing.T) {
			setVersion(t, tt.tack)
			f := newFixture(t)
			f.flux.Fail["Reconcile"] = errors.New("stop in Hold")
			if _, err := f.start("b", failoverv1.ModePlanned, engine.Options{}); !errors.Is(err, engine.ErrStopped) {
				t.Fatalf("run: %v", err)
			}
			delete(f.flux.Fail, "Reconcile")
			sf := f.ledger("b")
			sf.Status.WrittenBy = tt.writtenBy
			if err := f.env("b").Ledger.UpdateStatus(context.Background(), sf); err != nil {
				t.Fatal(err)
			}
			before := f.ledger("b")
			calls := len(f.flux.Calls)
			after, err := f.resume("b", engine.Options{})
			if !tt.refused {
				if err != nil || after.Status.WrittenBy != tt.tack {
					t.Fatalf("resume: %v, writtenBy %q, want %q", err, after.Status.WrittenBy, tt.tack)
				}
				return
			}
			if !errors.Is(err, engine.ErrNewerLedger) || !errors.Is(err, engine.ErrCannotResume) ||
				!strings.Contains(err.Error(), "run run-1 was last written by tack "+tt.writtenBy+", and this tack is "+tt.tack+", which is older") ||
				!strings.Contains(err.Error(), "use tack "+tt.writtenBy+" or later") {
				t.Fatalf("resume: %v, want a refusal that names both versions", err)
			}
			if after.ResourceVersion != before.ResourceVersion || len(f.flux.Calls) != calls {
				t.Errorf("the refused resume changed something: resourceVersion %s to %s, Flux %v", before.ResourceVersion, after.ResourceVersion, f.flux.Calls[calls:])
			}
		})
	}
}

// RFC0003 Requirement 24: every status write records this tack's version,
// the abandonment after the create among them.
func TestEveryWriteRecordsWrittenBy(t *testing.T) {
	setVersion(t, "v1.3.0")
	f := newFixture(t)
	l := &recording{Ledger: f.env("b").Ledger}
	env := f.env("b")
	env.Ledger = l
	if err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(l.versions) < 10 || slices.ContainsFunc(l.versions, func(v string) bool { return v != "v1.3.0" }) {
		t.Errorf("writes recorded %v, want v1.3.0 in each", l.versions)
	}
}

// A ledger that each earlier release wrote, stopped in Hold, resumes under
// this tack: the run finishes, keeps what the old tack recorded, and gets
// status.writtenBy and its history from its first write ([RFC#0003]
// Requirement 13).
func TestLedgersOfEarlierReleasesResume(t *testing.T) {
	setVersion(t, "v1.3.0")
	for _, file := range []string{
		"testdata/ledgers/v1.0.0/planned.yaml",
		"testdata/ledgers/v1.2.0/planned.yaml",
		"testdata/ledgers/v1.2.1/planned-mapped.yaml",
	} {
		t.Run(file, func(t *testing.T) {
			f := newFixture(t)
			sf := loadLedger(t, file)
			started := metav1.NewTime(time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC))
			sf.Status = failoverv1.SiteFailoverStatus{
				State: failoverv1.RunStopped, Reason: failoverv1.ReasonStepFailed, Message: "step reconcile-hold-a: timed out",
				StartedAt: &started, Captured: map[string]string{"note": "kept"},
				FollowUps: []failoverv1.FollowUp{{Site: "c", Action: "Rebuild", Reason: "kept"}},
			}
			for _, s := range sf.Spec.Steps {
				sf.Status.Steps = append(sf.Status.Steps, failoverv1.StepStatus{ID: s.ID, State: failoverv1.StepPending})
			}
			ledger := f.env("b").Ledger
			if err := ledger.Create(context.Background(), sf.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			stored, err := ledger.Get(context.Background(), sf.Name)
			if err != nil {
				t.Fatal(err)
			}
			stored.Status = sf.Status
			if err := ledger.UpdateStatus(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			if err := f.env("b").Resume(context.Background(), stored, engine.Options{}); err != nil {
				t.Fatalf("resume: %v", err)
			}
			got, err := ledger.Get(context.Background(), sf.Name)
			if err != nil {
				t.Fatal(err)
			}
			st := got.Status
			if st.State != failoverv1.RunSucceeded || st.WrittenBy != "v1.3.0" || !slices.Equal(entries(got), []string{"Running", "Succeeded"}) {
				t.Errorf("state %s, writtenBy %q, history %v", st.State, st.WrittenBy, entries(got))
			}
			if !st.StartedAt.Equal(&started) || st.Captured["note"] != "kept" || len(st.FollowUps) != 1 || st.Git.Commit == "" {
				t.Errorf("lost what the old tack recorded: startedAt %v, captured %v, follow-ups %v, commit %q", st.StartedAt, st.Captured, st.FollowUps, st.Git.Commit)
			}
		})
	}
}
