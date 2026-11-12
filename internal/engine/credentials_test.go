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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

// credClients are clients whose credentials take a while for some sites,
// and that log each credential fetch and each check. As client-go does,
// they keep the credentials they got: a later fetch for the site returns
// at once and is not logged.
type credClients struct {
	kube.Clients
	slow map[string]time.Duration

	mu      sync.Mutex
	log     []string
	running int
	overlap bool
	got     map[string]bool
}

func (c *credClients) add(e string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, e)
}

func (c *credClients) Credentials(_ context.Context, site string) error {
	c.mu.Lock()
	if c.got[site] {
		c.mu.Unlock()
		return nil
	}
	c.running++
	c.overlap = c.overlap || c.running > 1
	c.mu.Unlock()
	c.add("credentials " + site)
	time.Sleep(c.slow[site])
	c.mu.Lock()
	c.running--
	if c.got == nil {
		c.got = map[string]bool{}
	}
	c.got[site] = true
	c.mu.Unlock()
	return nil
}

func (c *credClients) Reachable(ctx context.Context, site string) error {
	c.add("check " + site)
	return c.Clients.Reachable(ctx, site)
}

// #58 row 6. The checks of a read start at once, and so did the sites'
// credential plugins, whose prompts mixed. A read now obtains each site's
// credentials one at a time, in the order of the Workload, before any
// check starts, and says when one takes a while ([RFC#0007] Requirement
// 12).
func TestCredentialsBeforeChecksInSiteOrder(t *testing.T) {
	defer func(d time.Duration) { engine.CredentialsWait = d }(engine.CredentialsWait)
	engine.CredentialsWait = 20 * time.Millisecond
	cc := &credClients{
		Clients: testenv.NewClients(map[string][]client.Object{"a": nil, "b": nil, "c": nil}),
		slow:    map[string]time.Duration{"b": 200 * time.Millisecond},
	}
	var out bytes.Buffer
	sc := &engine.SiteChecks{Clients: cc, Waits: []time.Duration{time.Millisecond, time.Millisecond}, Out: &out}
	spec := &failoverv1.WorkloadSpec{Sites: []failoverv1.Site{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	end := engine.CheckAhead(context.Background(), sc, spec)
	for _, s := range []string{"a", "b", "c"} {
		if err := sc.Reachable(context.Background(), s); err != nil {
			t.Fatalf("site %s: %v", s, err)
		}
	}
	end()
	if got := strings.Join(cc.log[:3], ", "); got != "credentials a, credentials b, credentials c" {
		t.Errorf("the reads began with %s, want the credentials of a, b and c, in order:\n%v", got, cc.log)
	}
	if cc.overlap {
		t.Error("two sites' credentials were obtained at once")
	}
	if !strings.Contains(out.String(), "  waiting   for the credentials of site b: the credential plugin of its kube context runs, or another site's does, with no time limit\n") ||
		strings.Contains(out.String(), "credentials of site a") {
		t.Errorf("want one waiting line, for site b:\n%s", out.String())
	}
}

// syncBuffer is a bytes.Buffer that progress lines from several goroutines
// may write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// expiring are clients whose credentials for site b expire once site a's
// Cluster declares pg-b primary, as during the wait for a's demotion: the
// next fetch takes slow, then fails when fail is set, else gives
// credentials that later fetches reuse.
type expiring struct {
	*testenv.Clients
	slow time.Duration
	fail bool

	mu      sync.Mutex
	fetched bool
}

func (c *expiring) Credentials(ctx context.Context, site string) error {
	if site != "b" || !c.demoted(ctx) {
		return nil
	}
	c.mu.Lock()
	again := c.fetched
	c.fetched = true
	c.mu.Unlock()
	if again && !c.fail {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.slow):
	}
	if c.fail {
		return errors.New(`site "b": the credentials of context "site-b" could not be obtained, so the site is not counted as lost: exit status 1`)
	}
	return nil
}

func (c *expiring) demoted(ctx context.Context) bool {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"})
	if err := c.Sites["a"].Get(ctx, client.ObjectKey{Namespace: "db", Name: "pg-a"}, u); err != nil {
		return false
	}
	p, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "primary")
	return p == "pg-b"
}

// Found in the coordinator's review of PR #68: credentials that expire
// during a run, such as while it waits for the demotion, were obtained
// inside the next step's request, where client-go runs the credential
// plugin with no context: a plugin that waited, such as for an identity
// provider that is down, held the run between the demotion and the
// promotion, with no line, and an interrupt did not end the wait. The run
// now obtains the site's credentials before each step, with the line,
// and stops before the step when they cannot be obtained ([RFC#0007]
// Requirement 12).
func TestRunObtainsCredentialsBeforeEachStep(t *testing.T) {
	defer func(d time.Duration) { engine.CredentialsWait = d }(engine.CredentialsWait)
	engine.CredentialsWait = 20 * time.Millisecond
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail %v", fail), func(t *testing.T) {
			w := workloadN(t, []string{"a", "b"})
			tc := testenv.NewClients(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go (&testenv.Operator{Clients: tc, Sites: []string{"a", "b"}}).Run(ctx)
			out := &syncBuffer{}
			env := &engine.Env{
				Clients: &expiring{Clients: tc, slow: 100 * time.Millisecond, fail: fail}, Driver: cnpg.Driver{},
				Flux: testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"), Git: &testenv.Git{},
				Ledger: &ledger.Store{Client: tc.Sites["b"], Namespace: "tack-system"}, Poll: time.Millisecond,
				ReachWaits: []time.Duration{time.Millisecond, time.Millisecond}, Out: out,
			}
			p, err := env.MakePlan(ctx, engine.Request{Run: "run-1", Workload: w.Name, Spec: &w.Spec,
				Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}})
			if err != nil {
				t.Fatal(err)
			}
			err = env.Start(ctx, p.SiteFailover, engine.Options{})
			sf, gerr := env.Ledger.Get(ctx, "run-1")
			if gerr != nil {
				t.Fatal(gerr)
			}
			if !strings.Contains(out.String(), "  waiting   for the credentials of site b: ") {
				t.Errorf("no waiting line for site b:\n%s", out.String())
			}
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"})
			if err := tc.Sites["b"].Get(ctx, client.ObjectKey{Namespace: "db", Name: "pg-b"}, u); err != nil {
				t.Fatal(err)
			}
			primary, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "primary")
			if !fail {
				if err != nil || sf.Status.State != failoverv1.RunSucceeded || primary != "pg-b" {
					t.Fatalf("run: %v, %s (%s), pg-b declares %s:\n%s", err, sf.Status.State, sf.Status.Message, primary, out.String())
				}
				return
			}
			if err == nil || sf.Status.State != failoverv1.RunStopped || primary != "pg-a" ||
				!strings.Contains(sf.Status.Message, `site b: site "b": the credentials of context "site-b" could not be obtained`) {
				t.Errorf("run: %v, %s (%s), pg-b declares %s; want a stop before the promotion:\n%s", err, sf.Status.State, sf.Status.Message, primary, out.String())
			}
			for _, st := range sf.Status.Steps {
				if st.ID == "promote-b" && (st.State != failoverv1.StepPending || st.Attempts != 0) {
					t.Errorf("promote-b: %s, %d attempts; want Pending, not started", st.State, st.Attempts)
				}
			}
		})
	}
}

// A check outside a read that obtains every site's credentials first, such
// as the check before a run's first step at a site, obtains them itself,
// apart from the check, with the line when they take a while (coordinator
// review of PR #68: no line came from such a check).
func TestCheckObtainsCredentialsWithALine(t *testing.T) {
	defer func(d time.Duration) { engine.CredentialsWait = d }(engine.CredentialsWait)
	engine.CredentialsWait = 20 * time.Millisecond
	cc := &credClients{
		Clients: testenv.NewClients(map[string][]client.Object{"a": nil, "b": nil}),
		slow:    map[string]time.Duration{"b": 200 * time.Millisecond},
	}
	out := &syncBuffer{}
	sc := &engine.SiteChecks{Clients: cc, Waits: []time.Duration{time.Millisecond, time.Millisecond}, Out: out}
	if err := sc.Reachable(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cc.log, ", "); got != "credentials b, check b" {
		t.Errorf("the check of b ran %s, want its credentials, then the check", got)
	}
	if !strings.Contains(out.String(), "  waiting   for the credentials of site b: ") {
		t.Errorf("no waiting line for site b:\n%s", out.String())
	}
}
