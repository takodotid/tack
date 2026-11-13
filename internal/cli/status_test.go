package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// [RFC#0009] Requirement 28: tack status RUN prints when writes stopped and
// came back, an estimate as one, and each step's earlier attempts.
func TestStatusShowsWritesDownAndAttempts(t *testing.T) {
	t0 := metav1.NewTime(time.Date(2026, 10, 27, 14, 31, 7, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(5*time.Minute + 45*time.Second))
	sf := &failoverv1.SiteFailover{}
	sf.Name, sf.Namespace = "pg-main-20261027-143107", "tack-system"
	sf.Spec.Steps = []failoverv1.Step{{ID: "hold-a", Phase: failoverv1.PhaseHold, Type: failoverv1.StepHold, Site: "a",
		Hold: &failoverv1.ObjectStep{Object: failoverv1.ObjectRef{Kind: "Cluster", Namespace: "db", Name: "pg-a"}}}}
	sf.Status.Writes = &failoverv1.WriteWindow{StoppedAt: &t0, ResumedAt: &t1}
	sf.Status.Steps = []failoverv1.StepStatus{{ID: "hold-a", State: failoverv1.StepSucceeded, Attempts: 2, StartedAt: &t1, FinishedAt: &t1,
		Earlier: []failoverv1.StepAttempt{{StartedAt: &t0, FinishedAt: &t0, State: failoverv1.StepFailed, Message: "hold refused"}}}}
	var b bytes.Buffer
	printStatus(&b, sf)
	for _, want := range []string{
		"Writes down: 2026-10-27 14:31:07Z to 2026-10-27 14:36:52Z (5m45s)\n",
		"        earlier attempt 1: Failed, started 2026-10-27 14:31:07Z, finished 2026-10-27 14:31:07Z: hold refused\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, b.String())
		}
	}
	sf.Status.Writes.StoppedEstimated = true
	b.Reset()
	printStatus(&b, sf)
	if want := "Writes down: by 2026-10-27 14:31:07Z at the latest (the run's start: the site was lost before), until 2026-10-27 14:36:52Z: at least 5m45s\n"; !strings.Contains(b.String(), want) {
		t.Errorf("status lacks %q:\n%s", want, b.String())
	}
}

// #94, [RFC#0004] Requirement 15: tack status prints the recorded last
// commit with the fork point. A fork recorded without it, as before,
// prints as before.
func TestStatusShowsTheForkLastCommit(t *testing.T) {
	ledger := func(fork string) *failoverv1.SiteFailover {
		t.Helper()
		sf := &failoverv1.SiteFailover{}
		if err := yaml.Unmarshal([]byte("status:\n  fork:\n"+fork), sf); err != nil {
			t.Fatal(err)
		}
		return sf
	}
	var b bytes.Buffer
	printStatus(&b, ledger("    site: b\n    position: 0/3000060\n    parentTimeline: 1\n    timeline: 2\n    lastCommit: \"2026-11-13T09:00:00.250000Z\"\n"))
	if want := "Fork: site b's new history leaves the old one at 0/3000060 on timeline 1 or just after; its last replayed commit is from 2026-11-13T09:00:00.25Z, by the old primary's clock; new timeline 2\n"; !strings.Contains(b.String(), want) {
		t.Errorf("status lacks %q:\n%s", want, b.String())
	}
	b.Reset()
	printStatus(&b, ledger("    site: b\n    position: 0/3000060\n    parentTimeline: 1\n    timeline: 2\n"))
	if want := "Fork: site b's new history leaves the old one at 0/3000060 on timeline 1 or just after; new timeline 2\n"; !strings.Contains(b.String(), want) {
		t.Errorf("status lacks %q:\n%s", want, b.String())
	}
}
