package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/testenv"
	"github.com/takodotid/tack/internal/version"
)

// stamp is how fixedNow starts a progress line.
const stamp = "2026-10-27T10:15:00Z "

// operate runs the fake CloudNativePG operator at sites a and b until the
// test ends.
func operate(t *testing.T, f *testFactory) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"a", "b"}}).Run(ctx)
}

// #35 row 29. Progress lines had no time, so an incident's timeline could
// not be rebuilt from a terminal's log. Each now starts with the time in
// UTC ([RFC#0009] Requirement 27); the plan and the prompt do not.
func TestProgressLinesAreStamped(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	operate(t, f)
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--yes")
	if code != ExitOK {
		t.Fatalf("run: exit %d:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	var progress []string
	for _, l := range lines {
		if strings.HasPrefix(l, stamp) {
			progress = append(progress, strings.TrimPrefix(l, stamp))
		}
	}
	for _, want := range []string{"ledger tack-system/pg-main-20261027-101500 created", "  start     hold-a-cluster-pg-a: ", "  done      demote-a: patched", "run pg-main-20261027-101500 succeeded: every step ran"} {
		if !slices.ContainsFunc(progress, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("no progress line %q with its time:\n%s", want, out)
		}
	}
	if !slices.Contains(lines, "Plan for workload pg-main: Planned, site a to site b") {
		t.Errorf("the plan's first line is not as printed before:\n%s", out)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, stamp+"Plan for workload") || strings.HasPrefix(l, "  start") || strings.HasPrefix(l, "ledger ") {
			t.Errorf("line %q: a plan line with a time, or a progress line without", l)
		}
	}
}

// #35 row 29. tack status RUN left out each step's times and attempts,
// and the run's earlier stops. It now prints each phase's and step's start
// and end, each step's attempts, and the history ([RFC#0009] Requirement
// 28).
func TestStatusShowsTimesAndHistory(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)}, "a")
	operate(t, f)
	f.git.Unreachable = true
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss", "--yes")
	if code != ExitWaiting {
		t.Fatalf("run: exit %d, want %d:\n%s", code, ExitWaiting, out)
	}
	f.git.Unreachable = false
	if out, code := run(t, f, "resume", "pg-main-20261027-101500", "-f", "testdata/workload.yaml", "--fence", "a", "--yes"); code != ExitOK {
		t.Fatalf("resume: exit %d:\n%s", code, out)
	}
	out, code = run(t, f, "status", "pg-main-20261027-101500", "-f", "testdata/workload.yaml")
	if code != ExitOK {
		t.Fatalf("status: exit %d:\n%s", code, out)
	}
	at := "2026-10-27 10:15:00Z"
	for _, want := range []string{
		"Hold  (started " + at + ", finished " + at + ")",
		"        started " + at + ", finished " + at + ", attempts 1\n",
		"        started " + at + ", finished " + at + ", attempts 2\n",
		"History:\n",
		"  " + at + "  Running  tack " + version.Version + ", user " + testenv.User + ", --yes\n",
		"  " + at + "  Waiting (GitUnreachable)  tack " + version.Version + ", user " + testenv.User + ", --yes\n      step git-commit: ",
		"  " + at + "  Running  tack " + version.Version + ", user " + testenv.User + ", --fence=a --yes\n",
		"  " + at + "  Succeeded  tack " + version.Version + ", user " + testenv.User + ", --fence=a --yes\n      every reachable step ran; ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	out, code = run(t, f, "status", "pg-main-20261027-101500", "-f", "testdata/workload.yaml", "-o", "yaml")
	if code != ExitOK || !strings.Contains(out, "  history:\n") || !strings.Contains(out, "user: "+testenv.User) {
		t.Errorf("status -o yaml: exit %d, no history:\n%s", code, out)
	}
}

// RFC0003 Requirements 22 and 23: tack abandon and the mark of a take-over append
// their entry to the run's history, with an Event.
func TestAbandonAndTakeOverRecordHistory(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), stoppedRun(t, "pg-main-earlier"), stoppedRun(t, "pg-main-other")),
	})
	if out, code := run(t, f, "abandon", "pg-main-other", "-f", "testdata/workload.yaml", "--reason", "a promoted again by hand", "--yes"); code != ExitOK {
		t.Fatalf("abandon: exit %d:\n%s", code, out)
	}
	h := ledgerAt(t, f, "b", "pg-main-other").Status.History
	if len(h) != 1 || h[0].State != failoverv1.HistoryAbandoned || h[0].Message != "a promoted again by hand" || h[0].User != testenv.User || !slices.Equal(h[0].Flags, []string{"--yes"}) {
		t.Errorf("history of the abandoned run: %+v", h)
	}
	operate(t, f)
	if out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--take-over", "pg-main-earlier", "--yes"); code != ExitOK {
		t.Fatalf("run: exit %d:\n%s", code, out)
	}
	h = ledgerAt(t, f, "b", "pg-main-earlier").Status.History
	if len(h) != 1 || h[0].State != failoverv1.HistoryTakenOver || h[0].Message != "run pg-main-20261027-101500 took this run over" || h[0].User != testenv.User ||
		!slices.Equal(h[0].Flags, []string{"--take-over=pg-main-earlier"}) {
		t.Errorf("history of the run taken over: %+v", h)
	}
	events := strings.Join(append(testenv.Events(f.clients.Sites["b"], "tack-system", "pg-main-other"), testenv.Events(f.clients.Sites["b"], "tack-system", "pg-main-earlier")...), "\n")
	for _, want := range []string{"Warning RunAbandoned: a promoted again by hand (tack ", "Normal RunTakenOver: run pg-main-20261027-101500 took this run over (tack "} {
		if !strings.Contains(events, want) {
			t.Errorf("no Event %q:\n%s", want, events)
		}
	}
}
