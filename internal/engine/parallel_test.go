package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/testenv"
)

// #35 row 48: sites were checked one after another, so each site that did
// not answer added its checks to every read. Each read now checks its
// sites at once ([RFC#0007] Requirement 10). testenv.AtOnce fails a first
// check that runs alone.

// The plan's read checks every site at once, and each once: its search
// for runs and the read after the confirmation reuse the checks.
func TestPlanChecksSitesAtOnce(t *testing.T) {
	s := newSites3(t)
	env := s.env("b")
	env.Clients = testenv.NewAtOnce(s.clients, "a", "b", "c")
	p, err := env.MakePlan(context.Background(), engine.Request{Run: "run-1", Workload: s.w.Name, Spec: &s.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := env.Recheck(context.Background(), p); err != nil {
		t.Fatalf("the read after the confirmation: %v", err)
	}
	for _, site := range []string{"a", "b", "c"} {
		if n := s.clients.Checks[site]; n != 1 {
			t.Errorf("site %s checked %d times, want 1", site, n)
		}
	}
}

// The read of the other sites checks the sites the process did not check
// before at once: in a resume before the promotion, a and c.
func TestReadOfOtherSitesChecksSitesAtOnce(t *testing.T) {
	s := newSites3(t)
	ctx := context.Background()
	env := s.env("b")
	env.Ledger = &diesBefore{Ledger: env.Ledger, step: "promote-b"}
	if err := env.Start(ctx, s.planned3("run-1"), engine.Options{}); err == nil {
		t.Fatal("the run went on after its process died")
	}
	env = s.env("b")
	env.Runs = nil
	env.Clients = testenv.NewAtOnce(s.clients, "a", "c")
	if err := env.Resume(ctx, s.ledger("b", "run-1"), engine.Options{}); err != nil {
		sf := s.ledger("b", "run-1")
		t.Fatalf("resume: %v; %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
}

// Checks at once keep their number per site (RFC0007 Requirement 5): 3 for
// each site that does not answer a planned plan, 1 for each lost site of a
// site-loss plan; the search for runs adds none.
func TestChecksAtOnceKeepTheirCount(t *testing.T) {
	for _, tt := range []struct {
		mode   failoverv1.Mode
		from   string
		checks map[string]int
	}{
		{failoverv1.ModePlanned, "a", map[string]int{"a": 1, "b": 1, "c": 3}},
		{failoverv1.ModeSiteLoss, "a", map[string]int{"a": 1, "b": 1, "c": 1}},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			s := newSites3(t)
			s.clients.SetDown("c", true)
			if tt.mode == failoverv1.ModeSiteLoss {
				s.clients.SetDown("a", true)
			}
			env := s.env("b")
			if _, err := env.MakePlan(context.Background(), engine.Request{Run: "run-1", Workload: s.w.Name, Spec: &s.w.Spec,
				Request: driver.Request{From: tt.from, To: "b", Mode: tt.mode}}); err != nil {
				t.Fatalf("plan: %v", err)
			}
			for site, want := range tt.checks {
				if n := s.clients.Checks[site]; n != want {
					t.Errorf("site %s checked %d times, want %d", site, n, want)
				}
			}
		})
	}
}

// A failure at one site reports as it did when sites were checked one
// after another: a blip at a, then a refused credential at b, gives a's
// lines, then b's error, and the same output on main.
func TestPlanFailureAtOneSiteReportsAsBefore(t *testing.T) {
	s := newSites3(t)
	s.clients.Blip("a", 1)
	s.clients.Fail("b", apierrors.NewUnauthorized("Unauthorized"))
	env := s.env("b")
	var out bytes.Buffer
	env.Out = &out
	_, err := env.MakePlan(context.Background(), engine.Request{Run: "run-1", Workload: s.w.Name, Spec: &s.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}})
	if err == nil || err.Error() != "site b: Unauthorized" {
		t.Fatalf("err %v, want site b: Unauthorized", err)
	}
	want := "  retry     site a did not answer check 1 of 3 (site a is unreachable: context deadline exceeded); next check in 1ms\n" +
		"  answered  site a on check 2 of 3\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// scripted checks sites a, b and c with a script per site, and lets a's
// last check answer only once the checks of b and c ended, as a slow site
// first in the Workload does. Its clock moves a second per read.
type scripted struct {
	mu     sync.Mutex
	script map[string][]bool
	calls  map[string]int
	clock  time.Time
	// last is closed once b and c gave their last answer.
	last chan struct{}
}

func newScripted() *scripted {
	return &scripted{
		script: map[string][]bool{"a": {false, true}, "b": {false, false, true}, "c": {false, true}},
		calls:  map[string]int{}, clock: time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC), last: make(chan struct{}),
	}
}

func (s *scripted) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = s.clock.Add(time.Second)
	return s.clock
}

func (s *scripted) Client(string) (client.Client, error) { return nil, errors.New("no client") }

func (s *scripted) Reachable(ctx context.Context, site string) error {
	s.mu.Lock()
	n := s.calls[site]
	s.calls[site]++
	answer := s.script[site][n]
	if s.calls["b"] == 3 && s.calls["c"] == 2 {
		select {
		case <-s.last:
		default:
			close(s.last)
		}
	}
	s.mu.Unlock()
	if site == "a" && answer {
		select {
		case <-s.last:
		case <-time.After(2 * time.Second):
		}
	}
	if !answer {
		return &kube.UnreachableError{Err: errors.New("connection refused")}
	}
	return nil
}

// stampLines records each line with the time it carries: the time it is
// written, or the one WriteStamped gives.
type stampLines struct {
	mu    sync.Mutex
	now   func() time.Time
	lines []string
}

func (w *stampLines) Write(p []byte) (int, error) { return w.WriteStamped(p, w.now()) }

func (w *stampLines) WriteStamped(p []byte, at time.Time) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range strings.SplitAfter(string(p), "\n") {
		if l != "" {
			w.lines = append(w.lines, at.Format("15:04:05")+" "+strings.TrimSpace(l))
		}
	}
	return len(p), nil
}

var threeSites = &failoverv1.WorkloadSpec{Sites: []failoverv1.Site{{Name: "a"}, {Name: "b"}, {Name: "c"}}}

// The lines of checks at once come in the order of the Workload's sites,
// as one site after another gave them: site c's checks end first and
// site a's last, yet a's lines come first. A line held back carries the
// time it was printed, not the time it went out.
func TestChecksAtOncePrintInSiteOrder(t *testing.T) {
	c := newScripted()
	out := &stampLines{now: c.now}
	checks := &engine.SiteChecks{Clients: c, Waits: []time.Duration{time.Millisecond, time.Millisecond}, Out: out, Now: c.now}
	ctx := context.Background()
	end := engine.CheckAhead(ctx, checks, threeSites)
	for _, site := range []string{"a", "b", "c"} {
		if err := checks.Reachable(ctx, site); err != nil {
			t.Fatalf("site %s: %v", site, err)
		}
	}
	end()
	var text []string
	at := map[string]string{}
	for _, l := range out.lines {
		stamp, line, _ := strings.Cut(l, " ")
		text = append(text, line)
		at[line] = stamp
	}
	want := []string{
		"retry     site a did not answer check 1 of 3 (connection refused); next check in 1ms",
		"answered  site a on check 2 of 3",
		"retry     site b did not answer check 1 of 3 (connection refused); next check in 1ms",
		"retry     site b did not answer check 2 of 3 (connection refused); next check in 1ms",
		"answered  site b on check 3 of 3",
		"retry     site c did not answer check 1 of 3 (connection refused); next check in 1ms",
		"answered  site c on check 2 of 3",
	}
	if strings.Join(text, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(out.lines, "\n"), strings.Join(want, "\n"))
	}
	// b and c answered before a: their lines, held back, carry earlier
	// times than a's answer.
	for _, l := range []string{want[4], want[6]} {
		if at[l] >= at[want[1]] {
			t.Errorf("%q at %s, after a's answer at %s: want the time it was printed", l, at[l], at[want[1]])
		}
	}
	if n := c.calls; n["a"] != 2 || n["b"] != 3 || n["c"] != 2 {
		t.Errorf("checks %v, want a 2, b 3, c 2", n)
	}
}

// A check that the end of its read cancels, after the read failed at
// another site, is not counted: the site did not fail to answer. In a
// site loss, where each site but to gets one check, counting it would
// make a later read in the process see the site as lost.
func TestCheckCancelledByTheReadIsNotCounted(t *testing.T) {
	refused := apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("refused"))
	c := &slowSite{refused: refused, released: make(chan struct{}), entered: make(chan struct{})}
	checks := &engine.SiteChecks{Clients: c, Waits: []time.Duration{time.Millisecond, time.Millisecond},
		Once: func(string) bool { return true }}
	ctx := context.Background()
	end := engine.CheckAhead(ctx, checks, threeSites)
	select {
	case <-c.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("site c's check did not start")
	}
	if err := checks.Reachable(ctx, "a"); err != nil {
		t.Fatalf("site a: %v", err)
	}
	if err := checks.Reachable(ctx, "b"); !apierrors.IsForbidden(err) {
		t.Fatalf("site b: %v, want its refusal", err)
	}
	// The read failed at b: its end cancels c's check, which has no answer
	// yet.
	start := time.Now()
	end()
	if d := time.Since(start); d > time.Second {
		t.Errorf("the end of the read waited %s for c's check", d)
	}
	close(c.released)
	if err := checks.Reachable(ctx, "c"); err != nil {
		t.Errorf("site c: %v, want an answer: the cancelled check counted as no answer", err)
	}
	if n := c.count("c"); n != 2 {
		t.Errorf("site c checked %d times, want 2: the cancelled one and the one after", n)
	}
}

// slowSite: a answers, b refuses the credential, and c's checks get no
// answer until released, as a site whose checks time out.
type slowSite struct {
	refused  error
	released chan struct{}
	// entered is closed when c's first check starts.
	entered chan struct{}
	mu      sync.Mutex
	calls   map[string]int
}

func (s *slowSite) count(site string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[site]
}

func (s *slowSite) Client(string) (client.Client, error) { return nil, errors.New("no client") }

func (s *slowSite) Reachable(ctx context.Context, site string) error {
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[site]++
	if site == "c" && s.calls[site] == 1 {
		close(s.entered)
	}
	s.mu.Unlock()
	switch site {
	case "b":
		return fmt.Errorf("site b answered its check with an error: %w", s.refused)
	case "c":
		select {
		case <-s.released:
		case <-ctx.Done():
			return &kube.UnreachableError{Err: ctx.Err()}
		}
	}
	return nil
}
