package engine_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

func siteChecks(c *testenv.Clients, waits ...time.Duration) (*engine.SiteChecks, *bytes.Buffer) {
	var out bytes.Buffer
	if waits == nil {
		waits = []time.Duration{time.Millisecond, time.Millisecond}
	}
	return &engine.SiteChecks{Clients: c, Waits: waits, Out: &out}, &out
}

func TestSiteChecksBounded(t *testing.T) {
	c := testenv.NewClients(nil)
	c.SetDown("a", true)
	checks, out := siteChecks(c)
	err := checks.Reachable(context.Background(), "a")
	if err == nil || !strings.HasPrefix(err.Error(), "no answer to 3 checks over ") {
		t.Fatalf("err %v", err)
	}
	if c.Checks["a"] != 3 {
		t.Errorf("checked %d times, want 3", c.Checks["a"])
	}
	if n := strings.Count(out.String(), "retry     site a did not answer check"); n != 2 {
		t.Errorf("%d retry lines:\n%s", n, out)
	}
	// The result is kept: a later call checks no more.
	if err2 := checks.Reachable(context.Background(), "a"); err2 == nil || err2.Error() != err.Error() || c.Checks["a"] != 3 {
		t.Errorf("second call: %v, %d checks", err2, c.Checks["a"])
	}
}

func TestSiteChecksAnswerNoted(t *testing.T) {
	c := testenv.NewClients(nil)
	c.Blip("a", 1)
	checks, out := siteChecks(c)
	if err := checks.Reachable(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := checks.Note("a"); got != "site a answered on check 2 of 3" {
		t.Errorf("note %q", got)
	}
	if !strings.Contains(out.String(), "answered  site a on check 2 of 3") {
		t.Errorf("output:\n%s", out)
	}
	if checks.Note("b") != "" {
		t.Error("a note for a site never checked")
	}
}

// One check for the sites Once names; a later call without once goes on
// with the same site's checks, and its pauses.
func TestSiteChecksOnceThenMore(t *testing.T) {
	c := testenv.NewClients(nil)
	c.SetDown("a", true)
	checks, out := siteChecks(c)
	checks.Once = func(site string) bool { return site == "a" }
	err := checks.Reachable(context.Background(), "a")
	if err == nil || strings.Contains(err.Error(), "no answer to") || c.Checks["a"] != 1 || out.Len() != 0 {
		t.Fatalf("once: %v, %d checks, output %q", err, c.Checks["a"], out)
	}
	c.SetDown("a", false)
	if err := checks.Check(context.Background(), "a", false); err != nil {
		t.Fatalf("more: %v", err)
	}
	if c.Checks["a"] != 2 || checks.Note("a") != "site a answered on check 2 of 3" {
		t.Errorf("%d checks, note %q", c.Checks["a"], checks.Note("a"))
	}
	if !strings.Contains(out.String(), "did not answer check 1 of 3") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSiteChecksInterrupted(t *testing.T) {
	c := testenv.NewClients(nil)
	c.SetDown("a", true)
	checks, _ := siteChecks(c, time.Hour, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := checks.Reachable(ctx, "a")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}
