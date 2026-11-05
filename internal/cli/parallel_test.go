package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/testenv"
)

// wrapFactory gives the commands' searches the clients that wrap returns.
type wrapFactory struct {
	*testFactory
	wrap func(*testenv.Clients) kube.Clients
}

func (f *wrapFactory) Clients(string, map[string]string) kube.Clients { return f.wrap(f.clients) }

func statusObjects() map[string][]client.Object {
	return map[string][]client.Object{
		"a": testenv.Site("a", "b", 0),
		"b": append(testenv.Site("b", "b", 2), siteLossRun("a", fixedNow.Add(-time.Hour))),
	}
}

// #35 row 48. The search for runs checks every site at once ([RFC#0007]
// Requirement 10): testenv.AtOnce fails a site checked alone.
func TestStatusSearchChecksSitesAtOnce(t *testing.T) {
	f := &wrapFactory{testFactory: newFactory(statusObjects()), wrap: func(c *testenv.Clients) kube.Clients {
		return testenv.NewAtOnce(c, "a", "b")
	}}
	out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitOK || !strings.Contains(out, "pg-main-earlier") {
		t.Fatalf("exit %d, want %d with the run:\n%s", code, ExitOK, out)
	}
}

// aLast lets a's second check answer only once b answered, up to 2 s, as
// a slow site first in the Workload does: checked at once, b's checks end
// first.
type aLast struct {
	*testenv.Clients
	once     sync.Once
	answered chan struct{}
	mu       sync.Mutex
	calls    map[string]int
}

func (c *aLast) Reachable(ctx context.Context, site string) error {
	c.mu.Lock()
	c.calls[site]++
	n := c.calls[site]
	c.mu.Unlock()
	if site == "a" && n == 2 {
		select {
		case <-c.answered:
		case <-time.After(2 * time.Second):
		}
	}
	err := c.Clients.Reachable(ctx, site)
	if site == "b" && err == nil {
		c.once.Do(func() { close(c.answered) })
	}
	return err
}

// The search's progress lines come in the order of the Workload's sites,
// as when it checked one site after another: a's, then b's, though b's
// checks end first. Each line starts with its time.
func TestStatusSearchPrintsSitesInOrder(t *testing.T) {
	f := &wrapFactory{testFactory: newFactory(statusObjects()), wrap: func(c *testenv.Clients) kube.Clients {
		return &aLast{Clients: c, answered: make(chan struct{}), calls: map[string]int{}}
	}}
	f.clients.Blip("a", 1)
	f.clients.Blip("b", 1)
	out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var progress []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, stamp) {
			progress = append(progress, strings.TrimPrefix(l, stamp))
		}
	}
	want := []string{
		"  retry     site a did not answer check 1 of 3 (site a is unreachable: context deadline exceeded); next check in 1ms",
		"  answered  site a on check 2 of 3",
		"  retry     site b did not answer check 1 of 3 (site b is unreachable: context deadline exceeded); next check in 1ms",
		"  answered  site b on check 2 of 3",
	}
	if strings.Join(progress, "\n") != strings.Join(want, "\n") {
		t.Errorf("progress:\n%s\nwant:\n%s\noutput:\n%s", strings.Join(progress, "\n"), strings.Join(want, "\n"), out)
	}
}

// A line that a site's checks held back goes out with the time it was
// printed ([RFC#0009] Requirement 27).
func TestStampedWritesHeldLinesWithTheirTime(t *testing.T) {
	var b bytes.Buffer
	w := &stamped{w: &b, now: func() time.Time { return fixedNow }}
	if _, err := w.WriteStamped([]byte("  retry     site b\n  answered  site b\n"), fixedNow.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("  answered  site a\n")); err != nil {
		t.Fatal(err)
	}
	want := "2026-10-27T10:14:30Z   retry     site b\n2026-10-27T10:14:30Z   answered  site b\n2026-10-27T10:15:00Z   answered  site a\n"
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}
