package engine

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/redact"
)

// DefaultReachWaits are the pauses between the checks of a site's API.
// With the 5 s timeout of each check, 3 checks take at most 30 s.
var DefaultReachWaits = []time.Duration{5 * time.Second, 10 * time.Second}

// SiteChecks are the checks of a site's API before Tack counts the site as
// unreachable ([RFC#0007] Requirement 5): up to 3, with pauses between
// them, or 1. It checks a site until it answers or its checks run out, and
// keeps the result: later calls return it. Its Reachable makes it the
// kube.Clients of one read, such as a plan, so each site gets one set of
// checks however often the read asks.
type SiteChecks struct {
	kube.Clients
	// Waits are the pauses between checks. Nil uses DefaultReachWaits.
	Waits []time.Duration
	// Once names the sites that Reachable checks once. Nil checks no site
	// once.
	Once func(site string) bool
	// Out receives a line for each retry and for an answer after one.
	Out io.Writer
	// Now returns the time. Nil uses time.Now.
	Now func() time.Time

	mu    sync.Mutex
	sites map[string]*siteCheck
}

type siteCheck struct {
	n          int
	err        error
	start, end time.Time
}

// SiteChecks returns the checks of one read for a run of the mode toward
// to: one check for a site other than to in a SiteLoss run, whose steps
// are deferrable ([RFC#0002]).
func (e *Env) SiteChecks(mode failoverv1.Mode, to string) *SiteChecks {
	return &SiteChecks{
		Clients: e.Clients, Waits: e.ReachWaits, Out: e.Out, Now: e.now,
		Once: func(site string) bool { return mode == failoverv1.ModeSiteLoss && site != to },
	}
}

// Reachable implements kube.Clients.
func (c *SiteChecks) Reachable(ctx context.Context, site string) error {
	return c.Check(ctx, site, c.Once != nil && c.Once(site))
}

// Check checks the site until it answers: up to len(Waits)+1 times, or once
// when once is set. A site that failed a single check gets the rest of its
// checks on a later call without once. Only a check that got no answer, or
// an answer that the API server is not ready yet, is repeated; any other
// error, such as a refused credential, is final. Only a site that gave no
// answer to its last check is unreachable (kube.IsUnreachable).
func (c *SiteChecks) Check(ctx context.Context, site string, once bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sites == nil {
		c.sites = map[string]*siteCheck{}
	}
	s := c.sites[site]
	if s == nil {
		s = &siteCheck{}
		c.sites[site] = s
	}
	waits := c.Waits
	if waits == nil {
		waits = DefaultReachWaits
	}
	checks := len(waits) + 1
	limit := checks
	if once {
		limit = 1
	}
	for s.n == 0 || s.err != nil && s.n < limit && kube.Retryable(s.err) {
		if s.n == 0 {
			s.start = c.now()
		} else {
			verb := "did not answer"
			if !kube.IsUnreachable(s.err) {
				verb = "answered not ready to"
			}
			c.printf("  retry     site %s %s check %d of %d (%v); next check in %s\n", site, verb, s.n, checks, s.err, waits[s.n-1])
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(waits[s.n-1]):
			}
		}
		s.err = c.Clients.Reachable(ctx, site)
		s.n++
		s.end = c.now()
		if s.err == nil && s.n > 1 {
			c.printf("  answered  site %s on check %d of %d\n", site, s.n, checks)
		}
		if s.err != nil && ctx.Err() != nil {
			return s.err
		}
	}
	switch {
	case s.err != nil && s.n > 1 && kube.IsUnreachable(s.err):
		return fmt.Errorf("no answer to %d checks over %s: %w", s.n, s.end.Sub(s.start).Round(time.Second), s.err)
	case s.err != nil && s.n > 1:
		return fmt.Errorf("%d checks over %s: %w", s.n, s.end.Sub(s.start).Round(time.Second), s.err)
	}
	return s.err
}

// Note says on which check the site answered, when a check failed before.
func (c *SiteChecks) Note(site string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sites[site]
	if s == nil || s.err != nil || s.n < 2 {
		return ""
	}
	waits := c.Waits
	if waits == nil {
		waits = DefaultReachWaits
	}
	return fmt.Sprintf("site %s answered on check %d of %d", site, s.n, len(waits)+1)
}

func (c *SiteChecks) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *SiteChecks) printf(format string, a ...any) {
	if c.Out != nil {
		fmt.Fprint(c.Out, redact.Text(fmt.Sprintf(format, a...)))
	}
}
