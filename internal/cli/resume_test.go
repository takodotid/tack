package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

// confirmQuestion starts the question of o.confirm.
const confirmQuestion = "Type the workload name"

// #38 item 3 (RFC0003 Requirements 18 and 20). tack resume refuses a run
// that cannot resume before it asks the operator: without a terminal it
// exits 1 with the reason, not 2 for the missing terminal, and with a
// terminal it never asks. Nothing is written.
func TestResumeRefusesBeforeAsking(t *testing.T) {
	t.Cleanup(func() { isTerminal, stdin = stdinIsTerminal, os.Stdin })
	for _, tt := range []struct {
		name string
		// at returns the ledgers at sites a and b; the run is
		// pg-main-earlier.
		at   func(t *testing.T) (a, b []client.Object)
		want string
	}{
		{name: "taken over", want: "run pg-main-earlier was taken over by run pg-main-later",
			at: func(t *testing.T) (a, b []client.Object) {
				old := stoppedRun(t, "pg-main-earlier")
				old.Status.SupersededBy = "pg-main-later"
				return nil, []client.Object{old}
			}},
		{name: "taken over by the other run's spec", want: "run pg-main-earlier was taken over by run pg-main-later",
			at: func(t *testing.T) (a, b []client.Object) {
				later := stoppedRun(t, "pg-main-later")
				later.Spec.TakeOver, later.Spec.To = "pg-main-earlier", "a"
				return []client.Object{later}, []client.Object{stoppedRun(t, "pg-main-earlier")}
			}},
		{name: "another run started after", want: "run pg-main-later started after run pg-main-earlier",
			at: func(t *testing.T) (a, b []client.Object) {
				later := stoppedRun(t, "pg-main-later")
				later.CreationTimestamp, later.Spec.To = metav1.NewTime(fixedNow), "a"
				return []client.Object{later}, []client.Object{stoppedRun(t, "pg-main-earlier")}
			}},
		{name: "never started", want: "run pg-main-earlier never recorded its start",
			at: func(t *testing.T) (a, b []client.Object) {
				never := stoppedRun(t, "pg-main-earlier")
				never.Status = failoverv1.SiteFailoverStatus{}
				return nil, []client.Object{never}
			}},
	} {
		for _, terminal := range []bool{false, true} {
			name := tt.name + ", no terminal"
			if terminal {
				name = tt.name + ", terminal"
			}
			t.Run(name, func(t *testing.T) {
				a, b := tt.at(t)
				f := newFactory(map[string][]client.Object{
					"a": append(testenv.Site("a", "a", 2), a...),
					"b": append(testenv.Site("b", "a", 0), b...),
				})
				isTerminal = func() bool { return terminal }
				stdin = strings.NewReader("pg-main\n")
				out, code := run(t, f, "resume", "pg-main-earlier", "-f", "testdata/workload.yaml")
				if code != ExitError || !strings.Contains(out, "the run cannot resume: "+tt.want) {
					t.Fatalf("exit %d, want %d with %q:\n%s", code, ExitError, tt.want, out)
				}
				if strings.Contains(out, confirmQuestion) || strings.Contains(out, "no terminal to confirm on") {
					t.Errorf("tack resume asked before it refused:\n%s", out)
				}
				if f.clients.Writes != 0 || len(f.flux.Calls) != 0 {
					t.Errorf("%d cluster writes, Flux %v", f.clients.Writes, f.flux.Calls)
				}
			})
		}
	}
}

// #38 item 3. A run that another run takes over while the operator
// confirms is still refused after the confirmation (issue #27): the search
// runs again, and changes nothing.
func TestResumeSearchesAgainAfterConfirmation(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), stoppedRun(t, "pg-main-earlier")),
	})
	typeAfter(t, func() {
		later := stoppedRun(t, "pg-main-later")
		later.Spec.TakeOver, later.Spec.To = "pg-main-earlier", "a"
		if err := f.clients.Sites["a"].Create(context.Background(), later); err != nil {
			t.Fatal(err)
		}
	})
	out, code := run(t, f, "resume", "pg-main-earlier", "-f", "testdata/workload.yaml")
	if !strings.Contains(out, confirmQuestion) {
		t.Fatalf("tack resume did not ask:\n%s", out)
	}
	if code != ExitError || !strings.Contains(out, "run pg-main-earlier was taken over by run pg-main-later") {
		t.Fatalf("exit %d, want %d with the take-over:\n%s", code, ExitError, out)
	}
	if len(f.flux.Calls) != 0 {
		t.Errorf("the resume went on: Flux %v", f.flux.Calls)
	}
	store := &ledger.Store{Client: f.clients.Sites["b"], Namespace: "tack-system"}
	if sf, err := store.Get(context.Background(), "pg-main-earlier"); err != nil || sf.Status.State != failoverv1.RunStopped {
		t.Errorf("the ledger was written: %v, state %s", err, sf.Status.State)
	}
}

// #38 item 3 (RFC0007 Requirement 5). The search after the confirmation
// reuses the site checks of the search before it: a site that does not
// answer is checked no more often than when tack resume searched once.
// The run is planned, so site a, its from, gets 1 check in the search for
// the named run, 3 in the search for other runs, and 3 before its step.
func TestResumeSearchChecksSitesOnce(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), stoppedRun(t, "pg-main-earlier")),
	}, "a")
	out, code := run(t, f, "resume", "pg-main-earlier", "-f", "testdata/workload.yaml", "--yes")
	if code != ExitStopped || !strings.Contains(out, "site a is unreachable") {
		t.Fatalf("exit %d, want %d with site a unreachable:\n%s", code, ExitStopped, out)
	}
	if n := f.clients.Checks["a"]; n != 7 {
		t.Errorf("site a checked %d times, want 7", n)
	}
}

// #35 row 37. tack resume refuses an --accept-git-head that is not the
// commit the run's GitChanged stop named, before it asks, with the usage
// exit code, and writes nothing.
func TestResumeRefusesAnotherHeadBeforeAsking(t *testing.T) {
	t.Cleanup(func() { isTerminal, stdin = stdinIsTerminal, os.Stdin })
	named := "2222222222222222222222222222222222222222"
	sf := stoppedRun(t, "pg-main-earlier")
	sf.Status.Reason, sf.Status.Git.ChangedRevision = failoverv1.ReasonGitChanged, named
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), sf),
	})
	isTerminal = func() bool { return true }
	stdin = strings.NewReader("pg-main\n")
	out, code := run(t, f, "resume", "pg-main-earlier", "-f", "testdata/workload.yaml", "--accept-git-head", "4444444")
	if code != ExitUsage || !strings.Contains(out, "--accept-git-head 4444444 is not the commit the run stopped on: the stop named "+named) {
		t.Fatalf("exit %d, want %d:\n%s", code, ExitUsage, out)
	}
	if strings.Contains(out, confirmQuestion) {
		t.Errorf("tack resume asked before it refused:\n%s", out)
	}
	if f.clients.Writes != 0 || len(f.flux.Calls) != 0 {
		t.Errorf("%d cluster writes, Flux %v", f.clients.Writes, f.flux.Calls)
	}
}

// #35 row 43. A ledger that an older Tack wrote may hold a URL's password
// in its messages. tack status, its YAML and the error of tack resume
// print none ([RFC#0009] Requirement 10).
func TestOutputRedactsOlderLedgers(t *testing.T) {
	const url = "https://user:tok3n-secret@git.example.invalid/gitops.git"
	sf := stoppedRun(t, "pg-main-earlier")
	sf.Status.Message = "step hold-a-cluster-pg-a: fetch " + url + ": 403"
	sf.Status.Steps[0].Message = "fetch " + url + ": 403"
	sf.Status.Abandoned = &failoverv1.Abandonment{At: metav1.NewTime(fixedNow), Reason: "the search failed: Get " + url + ": EOF"}
	for _, args := range [][]string{
		{"status", "pg-main-earlier"},
		{"status", "pg-main-earlier", "-o", "yaml"},
		{"resume", "pg-main-earlier", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), sf.DeepCopy())})
			out, _ := run(t, f, append(args, "-f", "testdata/workload.yaml")...)
			if strings.Contains(out, "tok3n-secret") {
				t.Errorf("the password is printed:\n%s", out)
			}
			if !strings.Contains(out, "https://***@git.example.invalid/gitops.git") {
				t.Errorf("no redacted URL:\n%s", out)
			}
		})
	}
}
