package testenv

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/takodotid/tack/internal/kube"
)

// AtOnce makes the first check of each of its sites wait until every one
// of them is in its first check, as when a read checks its sites at once.
// A first check that waits longer than Wait fails with an error that is
// not a lost site, so a read that checks one site after another fails.
// Checks of other sites, and later checks, go through as they are.
type AtOnce struct {
	kube.Clients
	// Wait bounds the wait of a first check.
	Wait time.Duration

	sites   []string
	mu      sync.Mutex
	entered map[string]bool
	all     chan struct{}
}

// NewAtOnce returns an AtOnce over c for sites, which waits up to 2 s.
func NewAtOnce(c kube.Clients, sites ...string) *AtOnce {
	return &AtOnce{Clients: c, Wait: 2 * time.Second, sites: sites, entered: map[string]bool{}, all: make(chan struct{})}
}

// Reachable implements kube.Clients.
func (a *AtOnce) Reachable(ctx context.Context, site string) error {
	a.mu.Lock()
	first := slices.Contains(a.sites, site) && !a.entered[site]
	if first {
		a.entered[site] = true
		if len(a.entered) == len(a.sites) {
			close(a.all)
		}
	}
	a.mu.Unlock()
	if first {
		select {
		case <-a.all:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.Wait):
			return fmt.Errorf("site %s was checked alone: the checks of %v did not run at once", site, a.sites)
		}
	}
	return a.Clients.Reachable(ctx, site)
}
