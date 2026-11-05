package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/testenv"
	"github.com/takodotid/tack/internal/version"
)

// entries returns a run's history as "State (Reason) flags", one per
// entry.
func entries(sf *failoverv1.SiteFailover) []string {
	var out []string
	for _, h := range sf.Status.History {
		s := string(h.State)
		if h.Reason != "" {
			s += " (" + h.Reason + ")"
		}
		if len(h.Flags) > 0 {
			s += " " + strings.Join(h.Flags, " ")
		}
		out = append(out, s)
	}
	return out
}

// hasEvent tells whether an Event on run-1 at the site starts with prefix.
func (f *fixture) hasEvent(site, prefix string) bool {
	return slices.ContainsFunc(testenv.Events(f.clients.Sites[site], "tack-system", "run-1"), func(e string) bool {
		return strings.HasPrefix(e, prefix)
	})
}

// #35 row 29. A resume cleared the run's reason and message, so an earlier
// stop disappeared, and no ledger recorded who ran a process, with which
// version and flags. Each state change now appends an entry to
// status.history, and each entry and step sends an Event ([RFC#0003]
// Requirements 22 and 23).
func TestHistoryRecordsEachStateChange(t *testing.T) {
	f := newFixture(t)
	plan := f.plan("b", failoverv1.ModePlanned)
	f.git.Unreachable = true
	sf, err := f.run("b", plan, engine.Options{Yes: true})
	if !errors.Is(err, engine.ErrWaiting) {
		t.Fatalf("run: %v, want a wait for Git", err)
	}
	firstStop := sf.Status.Message
	if _, err := f.resume("b", engine.Options{Fence: []string{"a"}}); !errors.Is(err, engine.ErrWaiting) {
		t.Fatalf("resume with Git down: %v", err)
	}
	f.git.Unreachable = false
	sf, err = f.resume("b", engine.Options{Yes: true})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	want := []string{
		"Running --yes", "Waiting (GitUnreachable) --yes",
		"Running --fence=a", "Waiting (GitUnreachable) --fence=a",
		"Running --yes", "Succeeded --yes",
	}
	if got := entries(sf); !slices.Equal(got, want) {
		t.Fatalf("history\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := sf.Status.History[1].Message; got != firstStop || !strings.Contains(got, "step git-commit") {
		t.Errorf("the first stop's entry has message %q, want the run's message then, %q", got, firstStop)
	}
	for i, h := range sf.Status.History {
		if h.Version != version.Version || h.User != testenv.User || h.Time.IsZero() {
			t.Errorf("entry %d: version %q, user %q, time %v; want %q, %q and a time", i, h.Version, h.User, h.Time, version.Version, testenv.User)
		}
	}
	if sf.Status.HistoryDropped != 0 {
		t.Errorf("historyDropped %d, want 0", sf.Status.HistoryDropped)
	}
	for _, e := range []string{
		"Normal RunRunning: (tack " + version.Version + ", user " + testenv.User + ", --yes)",
		"Warning RunWaiting: GitUnreachable: step git-commit:",
		"Normal RunRunning: (tack " + version.Version + ", user " + testenv.User + ", --fence=a)",
		"Normal StepStarted: hold-a-cluster-pg-a: ",
		"Normal StepSucceeded: demote-a",
		"Normal RunSucceeded: every step ran (tack ",
	} {
		if !f.hasEvent("b", e) {
			t.Errorf("no Event %q on the ledger; it has:\n%s", e, strings.Join(testenv.Events(f.clients.Sites["b"], "tack-system", "run-1"), "\n"))
		}
	}
}

// longFlux fails the check after release with a message far longer than
// a history entry holds, with a URL's password and a token in it.
type longFlux struct{ *testenv.Flux }

const longTail = " the server rejected promotionToken token-a-1 from " + secretURL

func (f longFlux) VerifyNoop(context.Context, string, []failoverv1.HeldObject, []failoverv1.ObjectField, bool, time.Duration) (string, error) {
	return "", errors.New(strings.Repeat("Kustomization flux-system/a is not Ready; ", 100) + longTail)
}

// RFC0003 Requirement 22: the history is bounded. A run stopped and
// resumed many times, each stop with a long message, keeps at most
// MaxHistory entries, each message within MaxHistoryMessage bytes and
// cleaned; it keeps the first entry, the newest ones, and the count of
// those it dropped. The whole ledger stays far below etcd's 1.5 MiB.
func TestHistoryStaysWithinBound(t *testing.T) {
	f := newFixture(t)
	env := f.env("b")
	env.Flux = longFlux{f.flux}
	if err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{TakeOver: []string{"run-0"}}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v, want a stop at the check after release", err)
	}
	// Each resume adds two entries: past MaxHistory after 50.
	const resumes = 60
	for i := 0; i < resumes; i++ {
		env := f.env("b")
		env.Flux = longFlux{f.flux}
		if err := env.Resume(context.Background(), f.ledger("b"), engine.Options{}); !errors.Is(err, engine.ErrStopped) {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	sf := f.ledger("b")
	h := sf.Status.History
	if len(h) != failoverv1.MaxHistory {
		t.Fatalf("%d entries, want %d", len(h), failoverv1.MaxHistory)
	}
	if want := int32(2*(resumes+1) - failoverv1.MaxHistory); sf.Status.HistoryDropped != want {
		t.Errorf("historyDropped %d, want %d", sf.Status.HistoryDropped, want)
	}
	if h[0].State != failoverv1.HistoryState(failoverv1.RunRunning) || !slices.Equal(h[0].Flags, []string{"--take-over=run-0"}) {
		t.Errorf("first entry %s %v, want the run's start with --take-over=run-0", h[0].State, h[0].Flags)
	}
	if last := h[len(h)-1]; last.State != failoverv1.HistoryState(failoverv1.RunStopped) {
		t.Errorf("last entry %s, want the last stop", last.State)
	}
	for i, e := range h {
		if len(e.Message) > failoverv1.MaxHistoryMessage {
			t.Errorf("entry %d: message of %d bytes", i, len(e.Message))
		}
		if strings.Contains(e.Message, "tok3n-secret") || strings.Contains(e.Message, "token-a-1") {
			t.Errorf("entry %d prints a password or a token: %s", i, e.Message)
		}
	}
	if m := h[len(h)-1].Message; !strings.HasSuffix(m, " [cut]") {
		t.Errorf("a long message is not marked as cut: ...%s", m[max(0, len(m)-40):])
	}
	b, err := json.Marshal(sf)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := json.Marshal(h)
	t.Logf("ledger %d bytes, history %d bytes", len(b), len(hb))
	if len(hb) > failoverv1.MaxHistory*1536 {
		t.Errorf("history of %d bytes, want at most %d", len(hb), failoverv1.MaxHistory*1536)
	}
	if len(b) > 256<<10 {
		t.Errorf("ledger of %d bytes, want at most 256 KiB", len(b))
	}
}

// AppendHistory drops the oldest entry but the first, and counts it.
func TestAppendHistory(t *testing.T) {
	var st failoverv1.SiteFailoverStatus
	for i := 0; i < failoverv1.MaxHistory+5; i++ {
		engine.AppendHistory(&st, failoverv1.HistoryEntry{Reason: string(rune('A' + i%26)), Message: strings.Repeat("x", i)})
	}
	if len(st.History) != failoverv1.MaxHistory || st.HistoryDropped != 5 {
		t.Fatalf("%d entries, %d dropped; want %d and 5", len(st.History), st.HistoryDropped, failoverv1.MaxHistory)
	}
	if len(st.History[0].Message) != 0 || len(st.History[1].Message) != 6 || len(st.History[len(st.History)-1].Message) != failoverv1.MaxHistory+4 {
		t.Errorf("kept entries %d, %d, ..., %d; want the first, then 6 onwards", len(st.History[0].Message), len(st.History[1].Message), len(st.History[len(st.History)-1].Message))
	}
}

// failingRecorder is a ledger whose Events fail, and whose site does not
// say who the operator is.
type failingRecorder struct {
	engine.Ledger
	events int
}

func (l *failingRecorder) Event(context.Context, *failoverv1.SiteFailover, bool, string, string, time.Time) error {
	l.events++
	return errors.New(`events is forbidden: User "operator@example.com" cannot create resource "events"`)
}

func (l *failingRecorder) User(context.Context) (string, error) {
	return "", errors.New("the server could not find the requested resource")
}

// RFC0003 Requirement 23: Events are best effort. A site that refuses
// them gets one attempt, and the run goes on; a site that does not say who
// the operator is leaves the history's user empty. Each says so once.
func TestEventsAndUserAreBestEffort(t *testing.T) {
	f := newFixture(t)
	var out bytes.Buffer
	env := f.env("b")
	rec := &failingRecorder{Ledger: env.Ledger}
	env.Ledger, env.Out = rec, &out
	if err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	sf := f.ledger("b")
	if got := entries(sf); !slices.Equal(got, []string{"Running", "Succeeded"}) {
		t.Errorf("history %v, want Running and Succeeded", got)
	}
	for _, h := range sf.Status.History {
		if h.User != "" {
			t.Errorf("user %q, want none", h.User)
		}
	}
	if rec.events != 1 {
		t.Errorf("%d Events tried, want 1", rec.events)
	}
	for _, note := range []string{"did not take an Event on run run-1", "did not say who runs this tack"} {
		if n := strings.Count(out.String(), note); n != 1 {
			t.Errorf("%d notes %q, want 1:\n%s", n, note, out.String())
		}
	}
}

// RFC0003 Requirement 8: a run that cannot search again after its create
// is abandoned, with an entry that says why and who, and no state.
func TestAbandonedAfterCreateRecordsHistory(t *testing.T) {
	f := newFixture(t)
	env := f.env("b")
	env.Runs = func(context.Context, kube.Clients) ([]failoverv1.SiteFailover, error) {
		return nil, errors.New("site a: list runs: the server is not ready")
	}
	if err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{Yes: true}); err == nil {
		t.Fatal("the run went on")
	}
	sf := f.ledger("b")
	if sf.Status.State != "" || sf.Status.Abandoned == nil {
		t.Fatalf("state %q, abandoned %v", sf.Status.State, sf.Status.Abandoned)
	}
	if got := entries(sf); !slices.Equal(got, []string{"Abandoned --yes"}) {
		t.Errorf("history %v, want one entry Abandoned --yes", got)
	}
	if h := sf.Status.History; len(h) == 1 && (h[0].Message != sf.Status.Abandoned.Reason || h[0].User != testenv.User) {
		t.Errorf("entry: message %q, user %q; want the reason %q and %q", h[0].Message, h[0].User, sf.Status.Abandoned.Reason, testenv.User)
	}
	if !f.hasEvent("b", "Warning RunAbandoned: the search for other runs after the ledger's creation failed") {
		t.Errorf("no Event RunAbandoned: %v", testenv.Events(f.clients.Sites["b"], "tack-system", "run-1"))
	}
}
