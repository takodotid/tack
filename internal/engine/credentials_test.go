package engine_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/testenv"
)

// credClients are clients whose credentials take a while for some sites,
// and that log each credential fetch and each check.
type credClients struct {
	kube.Clients
	slow map[string]time.Duration

	mu      sync.Mutex
	log     []string
	running int
	overlap bool
}

func (c *credClients) add(e string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, e)
}

func (c *credClients) Credentials(_ context.Context, site string) error {
	c.mu.Lock()
	c.running++
	c.overlap = c.overlap || c.running > 1
	c.mu.Unlock()
	c.add("credentials " + site)
	time.Sleep(c.slow[site])
	c.mu.Lock()
	c.running--
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
	if !strings.Contains(out.String(), "  waiting   for the credentials of site b: the credential plugin of its kube context runs, with no time limit\n") ||
		strings.Contains(out.String(), "credentials of site a") {
		t.Errorf("want one waiting line, for site b:\n%s", out.String())
	}
}
