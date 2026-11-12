package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

// slowChecks are clients whose checks of the down sites take a while, and
// that count how many such checks run at once.
type slowChecks struct {
	*testenv.Clients
	down map[string]bool

	mu       sync.Mutex
	counting bool
	running  int
	most     int
	checks   map[string]int
}

func (c *slowChecks) Reachable(ctx context.Context, site string) error {
	if !c.down[site] {
		return c.Clients.Reachable(ctx, site)
	}
	c.mu.Lock()
	counting := c.counting
	if counting {
		c.running++
		c.most = max(c.most, c.running)
		c.checks[site]++
	}
	c.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	err := c.Clients.Reachable(ctx, site)
	if counting {
		c.mu.Lock()
		c.running--
		c.mu.Unlock()
	}
	return err
}

// #58 row 4. In a planned run, the check before the first step of a site
// that is down took its 3 checks when the run reached that step, so each
// such site added its time one after another. The checks of every site
// whose steps are deferrable now start at once with the run; each site
// still gets 3, and its steps are deferred as before.
func TestChecksOfDeferrableSitesStartWithTheRun(t *testing.T) {
	sites := []string{"a", "b", "c", "d"}
	w := workloadN(t, sites)
	objs := map[string][]client.Object{}
	for _, s := range sites {
		n := int64(0)
		if s == "a" {
			n = 2
		}
		objs[s] = testenv.Site(s, "a", n)
	}
	tc := testenv.NewClients(objs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&testenv.Operator{Clients: tc, Sites: []string{"a", "b"}}).Run(ctx)
	tc.SetDown("c", true)
	tc.SetDown("d", true)
	clients := &slowChecks{Clients: tc, down: map[string]bool{"c": true, "d": true}, checks: map[string]int{}}
	env := &engine.Env{
		Clients: clients, Driver: cnpg.Driver{}, Flux: testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"), Git: &testenv.Git{},
		Ledger: &ledger.Store{Client: tc.Sites["b"], Namespace: "tack-system"}, Poll: time.Millisecond,
		ReachWaits: []time.Duration{time.Millisecond, time.Millisecond},
	}
	p, err := env.MakePlan(ctx, engine.Request{Run: "run-1", Workload: w.Name, Spec: &w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}})
	if err != nil {
		t.Fatal(err)
	}
	// Only the run's checks count: the plan checks every site at once.
	clients.mu.Lock()
	clients.counting = true
	clients.mu.Unlock()
	if err := env.Start(ctx, p.SiteFailover, engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	sf, err := env.Ledger.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run %s: %s", sf.Status.State, sf.Status.Message)
	}
	site := map[string]string{}
	for _, st := range sf.Spec.Steps {
		site[st.ID] = st.Site
	}
	deferred := map[string]bool{}
	for _, st := range sf.Status.Steps {
		if st.State == failoverv1.StepDeferred {
			deferred[site[st.ID]] = true
		}
	}
	if !deferred["c"] || !deferred["d"] || deferred["a"] || deferred["b"] {
		t.Errorf("deferred the steps of %v, want those of c and d", deferred)
	}
	if clients.most < 2 {
		t.Errorf("at most %d check of the sites that are down ran at once, want those of c and d together", clients.most)
	}
	for _, s := range []string{"c", "d"} {
		if n := clients.checks[s]; n != 3 {
			t.Errorf("site %s got %d checks in the run, want 3", s, n)
		}
	}
}
