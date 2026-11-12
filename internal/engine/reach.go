package engine

import (
	"context"
	"fmt"
	"io"
	"slices"
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
// checks however often the read asks. Calls for different sites run at
// once; calls for one site wait for each other. CheckAhead checks every
// site of a read at once ([RFC#0007] Requirement 10).
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
	// order holds back the lines of the sites that CheckAhead checks, to
	// write them in the order of the read's sites.
	order *ordered
}

type siteCheck struct {
	// mu is held while the site is checked.
	mu         sync.Mutex
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

func (c *SiteChecks) site(name string) *siteCheck {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sites == nil {
		c.sites = map[string]*siteCheck{}
	}
	s := c.sites[name]
	if s == nil {
		s = &siteCheck{}
		c.sites[name] = s
	}
	return s
}

// Check checks the site until it answers: up to len(Waits)+1 times, or once
// when once is set. A site that failed a single check gets the rest of its
// checks on a later call without once. Only a check that got no answer, or
// an answer that the API server is not ready yet, is repeated; any other
// error, such as a refused credential, is final. Only a site that gave no
// answer to its last check is unreachable (kube.IsUnreachable).
//
// A check that ends because ctx ended, by an interrupt or because the read
// that started it is done, is not counted, and neither is its pause: the
// site did not fail to answer. Once ctx ended, Check returns its error.
func (c *SiteChecks) Check(ctx context.Context, site string, once bool) error {
	s := c.site(site)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
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
		start := c.now()
		if s.n > 0 {
			verb := "did not answer"
			if !kube.IsUnreachable(s.err) {
				verb = "answered not ready to"
			}
			c.print(site, "  retry     site %s %s check %d of %d (%v); next check in %s\n", site, verb, s.n, checks, s.err, waits[s.n-1])
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(waits[s.n-1]):
			}
		}
		// The site's credentials first, apart from the check, with a line
		// when they take a while, also outside a read that obtains them
		// all first, such as before a run's first step at the site.
		err := c.obtain(ctx, site)
		if err == nil {
			err = c.Clients.Reachable(ctx, site)
		}
		if err != nil && ctx.Err() != nil {
			return err
		}
		if s.n == 0 {
			s.start = start
		}
		s.err = err
		s.n++
		s.end = c.now()
		if s.err == nil && s.n > 1 {
			c.print(site, "  answered  site %s on check %d of %d\n", site, s.n, checks)
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
	s := c.sites[site]
	c.mu.Unlock()
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.n < 2 {
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

// print writes a line of the site's checks, or holds it back while
// CheckAhead keeps the order of the sites.
func (c *SiteChecks) print(site, format string, a ...any) {
	if c.Out == nil {
		return
	}
	line := redact.Text(fmt.Sprintf(format, a...))
	c.mu.Lock()
	o := c.order
	c.mu.Unlock()
	if o != nil && o.write(site, line, c.now()) {
		return
	}
	io.WriteString(c.Out, line)
}

// aheadChecks are the kube.Clients of a read that keep each site's
// result: SiteChecks, and runChecks.
type aheadChecks interface {
	ahead(ctx context.Context, sites []string) (end func())
}

// CheckAhead starts the check of every site of spec at once through c,
// before a read that goes through the sites in order, such as the
// driver's Discover or a search for runs ([RFC#0007] Requirement 10). c
// keeps each site's result, so the read's own check of a site waits for
// that site's check and returns its result: the read's result and its
// error are those of a read that checks one site after another. The read
// calls end once it is done: end stops the checks still running, such as
// after the read failed at an earlier site, and waits for them. For a c
// that does not keep its results, CheckAhead starts nothing.
func CheckAhead(ctx context.Context, c kube.Clients, spec *failoverv1.WorkloadSpec) (end func()) {
	a, ok := c.(aheadChecks)
	if !ok {
		return func() {}
	}
	var sites []string
	for _, s := range spec.Sites {
		if !slices.Contains(sites, s.Name) {
			sites = append(sites, s.Name)
		}
	}
	return a.ahead(ctx, sites)
}

func (c *SiteChecks) ahead(ctx context.Context, sites []string) func() {
	c.credentials(ctx, sites)
	return c.checkAhead(ctx, sites, c.Reachable)
}

// CredentialsWait is how long a site's credentials may take before a
// progress line says that Tack waits for them ([RFC#0007] Requirement 12).
var CredentialsWait = 2 * time.Second

// Credentials implements kube.Credentialed when c's clients do.
func (c *SiteChecks) Credentials(ctx context.Context, site string) error {
	if cr, ok := c.Clients.(kube.Credentialed); ok {
		return cr.Credentials(ctx, site)
	}
	return nil
}

// obtain obtains the site's credentials, and prints a line once they took
// CredentialsWait, for the site's credential plugin or for another site's
// that runs ([RFC#0007] Requirement 12). It returns at once when c's
// clients do not obtain credentials apart from their checks.
func (c *SiteChecks) obtain(ctx context.Context, site string) error {
	if _, ok := c.Clients.(kube.Credentialed); !ok {
		return nil
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTimer(CredentialsWait)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			c.print(site, "  waiting   for the credentials of site %s: the credential plugin of its kube context runs, or another site's does, with no time limit\n", site)
		}
	}()
	err := c.Credentials(ctx, site)
	close(done)
	wg.Wait()
	return err
}

// credentials obtains the credentials of each site, one at a time, in the
// order of sites, before any of their checks starts ([RFC#0007]
// Requirement 12): a credential plugin may prompt on the terminal, and its
// time must not count toward a check. A failure is left to the site's
// check, which returns it.
func (c *SiteChecks) credentials(ctx context.Context, sites []string) {
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		_ = c.obtain(ctx, site)
	}
}

// checkAhead runs reach for each site in a goroutine of its own. Until
// they end, a site's progress lines go out while the checks of every site
// before it are done, and are held back otherwise, then written in the
// order of sites, each with the time it was printed ([RFC#0009]
// Requirement 27).
func (c *SiteChecks) checkAhead(ctx context.Context, sites []string, reach func(context.Context, string) error) func() {
	ctx, cancel := context.WithCancel(ctx)
	o := &ordered{out: c.Out, sites: sites, done: map[string]bool{}, held: map[string][]heldLine{}}
	c.mu.Lock()
	c.order = o
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, site := range sites {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer o.finish(site)
			_ = reach(ctx, site)
		}()
	}
	return func() {
		cancel()
		wg.Wait()
		c.mu.Lock()
		c.order = nil
		c.mu.Unlock()
	}
}

// ordered keeps the lines of checks that run at once in the order of
// their sites.
type ordered struct {
	mu    sync.Mutex
	out   io.Writer
	sites []string
	// next is the first site whose checks are not done. Its lines go out
	// at once; the lines of the sites after it are held.
	next int
	done map[string]bool
	held map[string][]heldLine
}

type heldLine struct {
	at   time.Time
	line string
}

// write writes or holds a line of site, printed at at. It returns false
// for a site that is not one of the sites.
func (o *ordered) write(site, line string, at time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	i := slices.Index(o.sites, site)
	switch {
	case i < 0:
		return false
	case i <= o.next:
		io.WriteString(o.out, line)
	default:
		o.held[site] = append(o.held[site], heldLine{at: at, line: line})
	}
	return true
}

// finish marks the checks of site done, and writes the held lines of each
// site whose turn comes.
func (o *ordered) finish(site string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.done[site] = true
	for o.next < len(o.sites) && o.done[o.sites[o.next]] {
		o.next++
		if o.next == len(o.sites) {
			break
		}
		for _, l := range o.held[o.sites[o.next]] {
			if w, ok := o.out.(StampWriter); ok {
				w.WriteStamped([]byte(l.line), l.at)
			} else {
				io.WriteString(o.out, l.line)
			}
		}
		delete(o.held, o.sites[o.next])
	}
}

// StampWriter is a writer of progress lines that starts each line with a
// time, such as the CLI's ([RFC#0009] Requirement 27). SiteChecks write
// the lines they held back to keep the order of the sites with the time
// they were printed, not the time they go out.
type StampWriter interface {
	io.Writer
	WriteStamped(p []byte, at time.Time) (int, error)
}
