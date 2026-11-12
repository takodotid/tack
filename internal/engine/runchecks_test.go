package engine

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/kube"
)

// orderClients log each credential fetch and each check, and note when two
// fetches overlapped. As client-go does, they keep the credentials they
// got: a later fetch for the site returns at once and is not logged. Every
// site answers.
type orderClients struct {
	slow map[string]time.Duration

	mu      sync.Mutex
	log     []string
	running int
	overlap bool
	got     map[string]bool
}

func (c *orderClients) add(e string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, e)
}

func (c *orderClients) Credentials(_ context.Context, site string) error {
	c.mu.Lock()
	if c.got[site] {
		c.mu.Unlock()
		return nil
	}
	c.running++
	c.overlap = c.overlap || c.running > 1
	c.log = append(c.log, "credentials "+site)
	c.mu.Unlock()
	time.Sleep(c.slow[site])
	c.mu.Lock()
	c.running--
	c.got[site] = true
	c.mu.Unlock()
	return nil
}

func (c *orderClients) Reachable(_ context.Context, site string) error {
	c.add("check " + site)
	return nil
}

func (c *orderClients) Client(string) (client.Client, error) { return nil, errors.New("no client") }

// Found in the coordinator's review of PR #68: no test failed without the
// pass that obtains the sites' credentials in a run's read that checks its
// sites at once, such as the read before the first Failover step. With it,
// the credentials of each site that the read checks come one at a time,
// in the order of the Workload, before any check; the site the read skips
// and a site that gave no answer in this process get none ([RFC#0007]
// Requirement 12).
func TestRunChecksObtainCredentialsFirst(t *testing.T) {
	cc := &orderClients{slow: map[string]time.Duration{"a": 200 * time.Millisecond}, got: map[string]bool{}}
	r := &runner{
		checks:    &SiteChecks{Clients: cc, Waits: []time.Duration{time.Millisecond, time.Millisecond}, Out: io.Discard},
		reachable: map[string]error{"d": &kube.UnreachableError{Err: errors.New("no answer")}},
	}
	spec := &failoverv1.WorkloadSpec{Sites: []failoverv1.Site{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}}
	c := runChecks{r: r, skip: "b"}
	ctx := context.Background()
	end := CheckAhead(ctx, c, spec)
	for _, s := range []string{"a", "c"} {
		if err := c.Reachable(ctx, s); err != nil {
			t.Fatalf("site %s: %v", s, err)
		}
	}
	end()
	if len(cc.log) < 2 || cc.log[0] != "credentials a" || cc.log[1] != "credentials c" {
		t.Errorf("the read began with %v, want the credentials of a, then c", cc.log)
	}
	if cc.overlap {
		t.Errorf("two sites' credentials were obtained at once: %v", cc.log)
	}
	for _, s := range []string{"b", "d"} {
		if slices.Contains(cc.log, "credentials "+s) || slices.Contains(cc.log, "check "+s) {
			t.Errorf("site %s, which the read does not check, got %v", s, cc.log)
		}
	}
}

// The checks that start with a run, of the sites whose steps are
// deferrable (#58 row 4), start at once: their credentials come first, one
// at a time, in the order of the steps ([RFC#0007] Requirement 12).
func TestDeferrableChecksObtainCredentialsFirst(t *testing.T) {
	cc := &orderClients{slow: map[string]time.Duration{"c": 200 * time.Millisecond}, got: map[string]bool{}}
	sf := &failoverv1.SiteFailover{}
	sf.Spec.Mode = failoverv1.ModeSiteLoss
	sf.Spec.Steps = []failoverv1.Step{
		{ID: "promote-b", Site: "b"},
		{ID: "declare-c", Site: "c", Deferrable: true},
		{ID: "declare-d", Site: "d", Deferrable: true},
	}
	r := &runner{
		sf:        sf,
		checks:    &SiteChecks{Clients: cc, Waits: []time.Duration{time.Millisecond, time.Millisecond}, Out: io.Discard},
		reachable: map[string]error{},
	}
	end := r.checkDeferrable(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		cc.mu.Lock()
		n := len(cc.log)
		cc.mu.Unlock()
		if n >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	end()
	if len(cc.log) != 4 || cc.log[0] != "credentials c" || cc.log[1] != "credentials d" {
		t.Errorf("the checks that start with the run ran %v, want the credentials of c, then d, then the checks", cc.log)
	}
	if cc.overlap {
		t.Errorf("two sites' credentials were obtained at once: %v", cc.log)
	}
}
