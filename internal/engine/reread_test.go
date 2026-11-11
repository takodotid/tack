package engine_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// failingClients are the fixture's clients, as the engine sees them: each
// read of a site's object asks fail first, with the site and "Kind
// namespace/name", and returns its error when it gives one. The fake
// operator reads through the fixture's own clients, so it sees no
// failure.
type failingClients struct {
	*testenv.Clients
	mu    sync.Mutex
	fail  func(site, what string, n int) error
	reads map[string]int
}

func (c *failingClients) Client(site string) (client.Client, error) {
	cl, err := c.Clients.Client(site)
	if err != nil {
		return nil, err
	}
	return &failingClient{Client: cl, site: site, c: c}, nil
}

// read counts a read of what at site, and returns fail's answer to it.
func (c *failingClients) read(site, what string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads == nil {
		c.reads = map[string]int{}
	}
	c.reads[site+" "+what]++
	return c.fail(site, what, c.reads[site+" "+what])
}

func (c *failingClients) count(site, what string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[site+" "+what]
}

type failingClient struct {
	client.Client
	site string
	c    *failingClients
}

func (f *failingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := f.c.read(f.site, obj.GetObjectKind().GroupVersionKind().Kind+" "+key.String()); err != nil {
		return err
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

// failNth fails the nth reads of what at site, as errs gives them by n.
func failNth(at, what string, errs map[int]error) func(site, w string, n int) error {
	return func(site, w string, n int) error {
		if site == at && w == what {
			return errs[n]
		}
		return nil
	}
}

// resumeWith resumes run-1 at b with clients and opts, and returns the
// ledger, the error and the progress lines.
func (f *fixture) resumeWith(clients *failingClients, opts engine.Options) (*failoverv1.SiteFailover, error, string) {
	f.t.Helper()
	var out bytes.Buffer
	env := f.env("b")
	env.Clients, env.Out = clients, &out
	err := env.Resume(context.Background(), f.ledger("b"), opts)
	return f.ledger("b"), err, out.String()
}

var unavailable = apierrors.NewServiceUnavailable("the server is currently unable to handle the request")

// #64. A planned run is killed before the promotion, the window with no
// writable primary. The resume's read of site a before the promotion
// fails once, after a answered its check: a connection reset, a 503 or a
// 429. The read is repeated with the pauses of a's checks, and the run
// promotes b. v1.4.0 stopped with StepFailed, and the window lasted until
// an operator resumed again.
func TestReadBeforePromotionRepeatedAfterBlip(t *testing.T) {
	for name, blip := range map[string]error{
		"connection reset": reset,
		"503":              unavailable,
		"429":              apierrors.NewTooManyRequests("slow down", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
			clients := &failingClients{Clients: f.clients, fail: failNth("a", "Cluster db/pg-a", map[int]error{1: blip})}
			sf, err, out := f.resumeWith(clients, engine.Options{})
			if err != nil || sf.Status.State != failoverv1.RunSucceeded {
				t.Fatalf("resume: %v, %s %s: %s\n%s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message, out)
			}
			for _, want := range []string{
				"  retry     site a, read Cluster db/pg-a, try 1 of 3 (",
				"  answered  site a, read Cluster db/pg-a, on try 2 of 3\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("no line %q:\n%s", want, out)
				}
			}
			if p, _ := f.cluster("b")["spec"].(map[string]any)["replica"].(map[string]any)["primary"].(string); p != "pg-b" {
				t.Errorf("Cluster pg-b declares %q primary, want pg-b", p)
			}
		})
	}
}

// #64. The read of a before the promotion fails on each of its 3 tries: the
// run stops with StepFailed before the promotion, which keeps its state,
// and b is unchanged. A repeat that runs out never counts the site as
// giving no answer, which would let the promotion go on ([RFC#0007]
// Requirement 9).
func TestReadBeforePromotionThatKeepsFailingStops(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	before := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b")
	clients := &failingClients{Clients: f.clients, fail: failNth("a", "Cluster db/pg-a", map[int]error{1: reset, 2: reset, 3: reset})}
	sf, err, _ := f.resumeWith(clients, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed ||
		!strings.HasPrefix(sf.Status.Message, "step promote-b: the read after the demotion: site a: read Cluster db/pg-a: 3 reads: ") &&
			!strings.HasPrefix(sf.Status.Message, "step promote-b: the read before the promotion: site a: read Cluster db/pg-a: 3 reads: ") {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if st := stepState(sf, "promote-b"); st != failoverv1.StepPending {
		t.Errorf("promote-b is %s, want Pending", st)
	}
	if after := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b"); after != before {
		t.Errorf("Cluster pg-b changed: resourceVersion %s, then %s", before, after)
	}
	if n := clients.count("a", "Cluster db/pg-a"); n != 3 {
		t.Errorf("%d reads of Cluster pg-a, want 3", n)
	}
}

// #64. A read that the API server refuses, such as a 403, or answers that
// the object is gone is not a blip: it is not repeated, and the run stops
// at once.
func TestReadRefusedIsNotRepeated(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, "pg-a", errors.New("no"))
	clients := &failingClients{Clients: f.clients, fail: failNth("a", "Cluster db/pg-a", map[int]error{1: forbidden, 2: forbidden, 3: forbidden})}
	sf, err, out := f.resumeWith(clients, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed || strings.Contains(sf.Status.Message, "reads:") {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if n := clients.count("a", "Cluster db/pg-a"); n != 1 || strings.Contains(out, "retry") {
		t.Errorf("%d reads of Cluster pg-a, want 1:\n%s", n, out)
	}
}

// #64. In the window with no writable primary, the steps' own reads are
// repeated too: the capture of a's demotion token, and the compare of it
// with its source before the promotion, each fail once after a's checks
// and reads before them passed. The run promotes b with the token.
func TestReadsOfStepsRepeatedAfterBlip(t *testing.T) {
	f := newFixture(t)
	f.diesAt(f.plan("b", failoverv1.ModePlanned), "capture-token", false)
	// Reads of Cluster pg-a in the resume, without a failure: 1 the read
	// after the demotion, 2 the capture, 3 the read before the promotion, 4
	// the compare of the token with its source, 5 to 9 the reads after the
	// promotion. With the failures, 3 repeats the capture, 5 is the compare
	// and 6 repeats it.
	clients := &failingClients{Clients: f.clients, fail: failNth("a", "Cluster db/pg-a", map[int]error{2: reset, 5: unavailable})}
	sf, err, out := f.resumeWith(clients, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v, %s %s: %s\n%s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message, out)
	}
	if n := strings.Count(out, "  answered  site a, read Cluster db/pg-a, on try 2 of 3\n"); n != 2 {
		t.Errorf("%d answers after a repeat, want 2:\n%s", n, out)
	}
	if n := clients.count("a", "Cluster db/pg-a"); n != 11 {
		t.Errorf("%d reads of Cluster pg-a, want 11", n)
	}
	if got := stepMessage(sf, "capture-token"); !strings.HasPrefix(got, "captured demotionToken") {
		t.Errorf("capture-token: %s", got)
	}
}

// flakyHolds is a Flux whose HeldBy fails with errs first, in order.
type flakyHolds struct {
	*testenv.Flux
	mu    sync.Mutex
	errs  []error
	calls int
}

func (f *flakyHolds) HeldBy(ctx context.Context, site string, obj failoverv1.ObjectRef) (string, error) {
	f.mu.Lock()
	f.calls++
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return f.Flux.HeldBy(ctx, site, obj)
}

// #64. The check of the hold right before the promotion's patch reads the
// object through Flux, and fails once: it is repeated, and the run
// promotes b.
func TestHoldReadBeforePatchRepeatedAfterBlip(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	var out bytes.Buffer
	env := f.env("b")
	flux := &flakyHolds{Flux: f.flux, errs: []error{reset}}
	env.Flux, env.Out = flux, &out
	if err := env.Resume(context.Background(), f.ledger("b"), engine.Options{}); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "  retry     site b, read the hold on Cluster db/pg-b, try 1 of 3 (") {
		t.Errorf("no retry line:\n%s", out.String())
	}
}

// #64. An interrupt during the pause of a repeated read stops the run
// before the step, which keeps its state, as an interrupt during a check
// does.
func TestInterruptDuringReadRepeatKeepsStep(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clients := &failingClients{Clients: f.clients, fail: func(site, what string, n int) error {
		if site == "a" && what == "Cluster db/pg-a" {
			time.AfterFunc(50*time.Millisecond, cancel)
			return reset
		}
		return nil
	}}
	env := f.env("b")
	env.Clients, env.ReachWaits = clients, []time.Duration{time.Hour, time.Hour}
	err := env.Resume(ctx, f.ledger("b"), engine.Options{})
	sf := f.ledger("b")
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed || !strings.Contains(sf.Status.Message, "context canceled") {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if st := stepState(sf, "promote-b"); st != failoverv1.StepPending {
		t.Errorf("promote-b is %s, want Pending", st)
	}
}

// lostSiteBack runs a site loss toward b with a down, then brings a back.
func (f *fixture) lostSiteBack() {
	f.t.Helper()
	f.clients.SetDown("a", true)
	if sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{}); err != nil {
		f.t.Fatalf("run: %v %s", err, sf.Status.Message)
	}
	f.clients.SetDown("a", false)
}

// #64, RFC0007 Requirement 6: the read before a site's steps reads that
// site only, since it does not use the state of any other. In a resume
// after a site loss, a comes back, and only a's steps are left: b gets no
// check and no read. v1.4.0 checked and read b in the read before a's
// steps.
func TestReadBeforeStepsReadsOnlyItsSite(t *testing.T) {
	f := newFixture(t)
	f.lostSiteBack()
	before := f.clients.Checks["b"]
	clients := &failingClients{Clients: f.clients, fail: func(string, string, int) error { return nil }}
	sf, err, out := f.resumeWith(clients, engine.Options{Fence: []string{"a"}})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s\n%s", err, sf.Status.State, sf.Status.Message, out)
	}
	if n := f.clients.Checks["b"] - before; n != 0 {
		t.Errorf("site b checked %d times in the resume, want 0", n)
	}
	if n := clients.count("b", "Cluster db/pg-b"); n != 0 {
		t.Errorf("Cluster pg-b read %d times, want 0", n)
	}
	if st := stepState(sf, "fence-a"); st != failoverv1.StepSucceeded {
		t.Errorf("fence-a is %s", st)
	}
}

// #64. A read error at a site that a step does not need does not stop it:
// b's Cluster refuses every read, and the resume still holds and fences a,
// the site that came back. v1.4.0 stopped before a's steps with StepFailed
// on b's error.
func TestReadBeforeStepsIgnoresErrorAtAnotherSite(t *testing.T) {
	f := newFixture(t)
	f.lostSiteBack()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, "pg-b", errors.New("no"))
	clients := &failingClients{Clients: f.clients, fail: func(site, what string, _ int) error {
		if site == "b" && what == "Cluster db/pg-b" {
			return forbidden
		}
		return nil
	}}
	sf, err, out := f.resumeWith(clients, engine.Options{Fence: []string{"a"}})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s: %s\n%s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message, out)
	}
	for _, id := range []string{"hold-a-cluster-pg-a", "fence-a"} {
		if st := stepState(sf, id); st != failoverv1.StepSucceeded {
			t.Errorf("%s is %s", id, st)
		}
	}
}
