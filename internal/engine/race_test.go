package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
)

// allRuns lists the ledgers at sites a and b, as tack's search does.
func (f *fixture) allRuns(ctx context.Context, _ kube.Clients) ([]failoverv1.SiteFailover, error) {
	var out []failoverv1.SiteFailover
	for _, site := range []string{"a", "b"} {
		l, err := (&ledger.Store{Client: f.clients.Sites[site], Namespace: "tack-system"}).List(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, l...)
	}
	return out, nil
}

// otherStarts is the ledger at site b, where another tack process creates
// its run of the workload, run-0, just before this process creates run-1.
// Both processes searched before their create, and found no active run.
// With state set, the other process also recorded its start.
type otherStarts struct {
	engine.Ledger
	other *failoverv1.SiteFailover
	state failoverv1.RunState
}

func (l *otherStarts) Create(ctx context.Context, sf *failoverv1.SiteFailover) error {
	if o := l.other; o != nil {
		l.other = nil
		if err := l.Ledger.Create(ctx, o); err != nil {
			return err
		}
		if l.state != "" {
			o.Status.State = l.state
			if err := l.Ledger.UpdateStatus(ctx, o); err != nil {
				return err
			}
		}
	}
	return l.Ledger.Create(ctx, sf)
}

// raceRun starts run-1 while another process creates run-0 with the same
// plan, and returns run-1's error.
func raceRun(t *testing.T, f *fixture, state failoverv1.RunState) error {
	t.Helper()
	plan := f.plan("b", failoverv1.ModePlanned)
	other := plan.DeepCopy()
	other.Name = "run-0"
	env := f.env("b")
	env.Ledger = &otherStarts{Ledger: env.Ledger, other: other, state: state}
	env.Runs = f.allRuns
	return env.Start(context.Background(), plan, engine.Options{})
}

// #35 row 35 (RFC0003 Requirement 8). Two tack processes find no active
// run, then both create a ledger. The one that searches again after its
// create and finds the other ends before its first status write: no step,
// no hold, no change. Its ledger is marked abandoned, so it blocks no new
// run, and never resumes.
func TestRunThatLosesTheRaceEndsBeforeItsSteps(t *testing.T) {
	for _, state := range []failoverv1.RunState{"", failoverv1.RunRunning} {
		name := string(state)
		if name == "" {
			name = "other not started yet"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			err := raceRun(t, f, state)
			if err == nil || !strings.Contains(err.Error(), "run run-0 of the workload") || !strings.Contains(err.Error(), "Run run-1 ran no step, changed nothing, and is abandoned") {
				t.Fatalf("err %v", err)
			}
			if len(f.flux.Calls) != 0 {
				t.Errorf("Flux calls: %v", f.flux.Calls)
			}
			if got := field(f.cluster("a"), "spec", "replica", "primary"); got != "pg-a" {
				t.Errorf("site a declares %v", got)
			}
			sf := f.ledger("b")
			if sf.Status.Abandoned == nil || sf.Status.State != "" || len(sf.Status.Steps) != 0 {
				t.Fatalf("run-1: abandoned %v, state %q, %d steps", sf.Status.Abandoned, sf.Status.State, len(sf.Status.Steps))
			}
			if err := f.env("b").Resume(context.Background(), sf, engine.Options{}); !errors.Is(err, engine.ErrCannotResume) || !strings.Contains(err.Error(), "run run-1 was abandoned") {
				t.Errorf("resume of the abandoned run: %v", err)
			}
		})
	}
}

// #35 row 35 (RFC0003 Requirement 8). A run that cannot search for other
// runs once its ledger exists cannot tell that no other run started: it
// ends the same way, before its first status write.
func TestRunThatCannotSearchAgainEnds(t *testing.T) {
	f := newFixture(t)
	env := f.env("b")
	searches := 0
	env.Runs = func(context.Context, kube.Clients) ([]failoverv1.SiteFailover, error) {
		searches++
		return nil, errors.New("site a: list runs: the server is not ready")
	}
	err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
	if err == nil || !strings.Contains(err.Error(), "the search for other runs after the ledger's creation failed: site a: list runs") ||
		!strings.Contains(err.Error(), "is abandoned") {
		t.Fatalf("err %v", err)
	}
	if searches != 1 || len(f.flux.Calls) != 0 {
		t.Errorf("%d searches, Flux calls %v", searches, f.flux.Calls)
	}
	if sf := f.ledger("b"); sf.Status.Abandoned == nil || sf.Status.State != "" || len(sf.Status.Steps) != 0 {
		t.Errorf("abandoned %v, state %q, %d steps", sf.Status.Abandoned, sf.Status.State, len(sf.Status.Steps))
	}
}

// #35 row 35. The run that won goes on: the abandoned run, created after
// it, ran no step, so it neither stops the winner's start nor refuses its
// resume as a newer run.
func TestRunThatWinsTheRaceGoesOn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	loser := f.plan("b", failoverv1.ModePlanned)
	loser.Name = "run-2"
	loser.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
	if err := f.clients.Sites["b"].Create(ctx, loser); err != nil {
		t.Fatal(err)
	}
	loser.Status = failoverv1.SiteFailoverStatus{Abandoned: &failoverv1.Abandonment{At: metav1.Now(), Reason: "run run-1 of the workload (not started) was created at the same time"}}
	if err := f.clients.Sites["b"].Status().Update(ctx, loser); err != nil {
		t.Fatal(err)
	}
	f.flux.Fail["VerifyGit"] = errors.New("stop at the check before release")
	env := f.env("b")
	env.Runs = f.allRuns
	if err := env.Start(ctx, f.plan("b", failoverv1.ModePlanned), engine.Options{}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run-1: %v", err)
	}
	delete(f.flux.Fail, "VerifyGit")
	if err := env.Resume(ctx, f.ledger("b"), engine.Options{}); err != nil {
		t.Fatalf("resume run-1: %v", err)
	}
	if sf := f.ledger("b"); sf.Status.State != failoverv1.RunSucceeded {
		t.Errorf("run-1 is %s: %s", sf.Status.State, sf.Status.Message)
	}
}
