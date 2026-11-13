package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/testenv"
)

// #75 row 1, [RFC#0003] Requirement 29: a planned run records when writes
// stopped, at the start of the old Pooler's scale to 0, and when they came
// back, at the end of the new Pooler's wait.
func TestPlannedRunRecordsWritesDown(t *testing.T) {
	f := newFixture(t)
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	w := sf.Status.Writes
	if w == nil || w.StoppedEstimated || w.StoppedAt == nil || w.ResumedAt == nil {
		t.Fatalf("writes %+v", w)
	}
	if got, want := w.StoppedAt, stepOf(sf, "pooler-a").StartedAt; !got.Equal(want) {
		t.Errorf("stoppedAt %v, want the start of pooler-a %v", got, want)
	}
	if got, want := w.ResumedAt, stepOf(sf, "wait-pooler-b").FinishedAt; !got.Equal(want) {
		t.Errorf("resumedAt %v, want the end of wait-pooler-b %v", got, want)
	}
}

// In a site loss, writes stopped when the site was lost, which Tack cannot
// see: the record is the run's start, marked as an estimate.
func TestSiteLossRecordsWritesDownAsEstimate(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	w := sf.Status.Writes
	if w == nil || !w.StoppedEstimated || !w.StoppedAt.Equal(sf.Status.StartedAt) || !w.ResumedAt.Equal(stepOf(sf, "wait-pooler-b").FinishedAt) {
		t.Errorf("writes %+v, want the run's start, estimated, to the end of wait-pooler-b", w)
	}
}

// #75 row 1: a step that runs again keeps its earlier attempts.
func TestStepKeepsEarlierAttempts(t *testing.T) {
	f := newFixture(t)
	f.flux.Fail["Hold"] = errors.New("hold refused")
	if _, err := f.start("b", failoverv1.ModePlanned, engine.Options{}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v, want a stop", err)
	}
	delete(f.flux.Fail, "Hold")
	sf, err := f.resume("b", engine.Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	st := stepOf(sf, "hold-a-cluster-pg-a")
	if st.Attempts != 2 || len(st.Earlier) != 1 || st.Earlier[0].State != failoverv1.StepFailed || !strings.Contains(st.Earlier[0].Message, "hold refused") || st.Earlier[0].StartedAt == nil {
		t.Errorf("step %+v, want one earlier attempt that failed", st)
	}
}

// stalled is the cnpg driver with a replica whose replay stands still.
type stalled struct{ cnpg.Driver }

func (stalled) Progress(context.Context, *failoverv1.WorkloadSpec, kube.Clients, *failoverv1.Step, map[string]string) (string, bool) {
	return "site b's replay at 0/3000000, short of 0/3000060 that the demotion token needs", true
}

// #75 row 2, [RFC#0004] Requirement 18: the wait for a planned promotion
// stops before its timeout when replay stands still short of the token.
func TestPromotionWaitStopsWhenReplayStalls(t *testing.T) {
	f := newFixture(t)
	f.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Only site a's CloudNativePG runs: b never promotes.
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"a"}}).Run(ctx)
	sf := f.plan("b", failoverv1.ModePlanned)
	for i := range sf.Spec.Steps {
		if s := &sf.Spec.Steps[i]; s.ID == "wait-promoted-b" {
			if s.Wait.Stall == nil || s.Wait.Stall.Duration != cnpg.StallWithin {
				t.Fatalf("wait-promoted-b has stall %v, want %s", s.Wait.Stall, cnpg.StallWithin)
			}
			s.Wait.Stall = &metav1.Duration{Duration: 50 * time.Millisecond}
		}
	}
	env := f.env("b")
	env.Driver = stalled{}
	start := time.Now()
	err := env.Start(context.Background(), sf, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v, want a stop", err)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("took %s: the wait ran to its timeout", took)
	}
	got := f.ledger("b")
	if st := stepOf(got, "wait-promoted-b"); st.State != failoverv1.StepFailed || !strings.Contains(st.Message, "stopped before the timeout: site b's replay at 0/3000000, short of 0/3000060 that the demotion token needs, has not moved for 50ms") {
		t.Errorf("wait-promoted-b: %+v", st)
	}
}

var _ driver.Progresser = stalled{}
