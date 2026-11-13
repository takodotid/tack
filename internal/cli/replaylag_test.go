package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/takodotid/tack/internal/driver"
)

// #72, [RFC#0004] Requirement 13: the plan prints each replica's replay
// lag, on a pipe and on a terminal, and never prints a lag of 0 as a time.
func TestReplayLagPrinted(t *testing.T) {
	zero, lag := time.Duration(0), 130*time.Second
	topo := &driver.Topology{Sites: []driver.SiteState{
		{Name: "a", Reachable: true, Role: driver.RolePrimary, Position: &driver.Position{Current: "0/5000000", Timeline: 1, Standing: "primary"}},
		{Name: "b", Reachable: true, Role: driver.RoleReplica, Position: &driver.Position{Received: "0/5000000", Replayed: "0/3000000", Timeline: 1, ReplayLag: &lag, Standing: "most caught up"}},
		{Name: "c", Reachable: true, Role: driver.RoleReplica, Position: &driver.Position{Received: "0/3000000", Replayed: "0/3000000", Timeline: 1, ReplayLag: &zero, Standing: "behind b by 32.0 MiB"}},
		{Name: "d", Reachable: true, Role: driver.RoleReplica, Position: &driver.Position{Received: "0/3000000", Replayed: "0/3000000", Timeline: 1, LagError: "forbidden", Standing: "behind b by 32.0 MiB"}},
	}}
	var pipe bytes.Buffer
	printPositions(&pipe, topo)
	columns := regexp.MustCompile(`  +`).ReplaceAllString(pipe.String(), "|")
	for _, want := range []string{
		"|a|writes at 0/5000000|timeline 1|primary\n",
		"|b|received 0/5000000|replayed 0/3000000|timeline 1, not streaming|last replayed commit 2m10s ago|most caught up\n",
		"|c|received 0/3000000|replayed 0/3000000|timeline 1, not streaming|replayed all it received|behind b by 32.0 MiB\n",
		"|d|received 0/3000000|replayed 0/3000000|timeline 1, not streaming|replay lag not read|behind b by 32.0 MiB\n",
	} {
		if !strings.Contains(columns, want) {
			t.Errorf("on a pipe, the positions\n%s\ndo not hold %q", pipe.String(), want)
		}
	}
	if strings.Contains(pipe.String(), " 0s") {
		t.Errorf("a lag of 0 printed as a time:\n%s", pipe.String())
	}

	term := &screen{width: 200}
	printPositions(term, topo)
	out := ansi.ReplaceAllString(term.String(), "")
	for _, want := range []string{"│ Replay ", "│ last replayed commit 2m10s ago │", "│ replayed all it received ", "│ replay lag not read "} {
		if !strings.Contains(out, want) {
			t.Errorf("on a terminal, the positions\n%s\ndo not hold %q", out, want)
		}
	}
}

// #94: with Tack's replay-time query, the replay column holds the last
// replayed commit's time, in RFC 3339 to the millisecond, and its age.
func TestReplayTimePrinted(t *testing.T) {
	zero := time.Duration(0)
	at := time.Date(2026, 11, 13, 9, 0, 0, 500_000_000, time.UTC)
	topo := &driver.Topology{Sites: []driver.SiteState{
		{Name: "b", Reachable: true, Role: driver.RoleReplica, Position: &driver.Position{Received: "0/3000000", Replayed: "0/3000000", Timeline: 1, ReplayLag: &zero, LastCommit: &at, CommitAge: time.Hour, Standing: "most caught up"}},
		{Name: "c", Reachable: true, Role: driver.RoleReplica, Position: &driver.Position{Received: "0/3000000", Replayed: "0/3000000", Timeline: 1, ReplayLag: &zero, LastCommit: &at, CommitAge: -time.Minute, Standing: "most caught up"}},
		{Name: "d", Reachable: true, Role: driver.RoleReplica, Position: &driver.Position{Received: "0/3000000", Replayed: "0/3000000", Timeline: 1, ReplayLag: &zero, CommitNote: "replayed no transaction since it started", Standing: "most caught up"}},
	}}
	var pipe bytes.Buffer
	printPositions(&pipe, topo)
	columns := regexp.MustCompile(`  +`).ReplaceAllString(pipe.String(), "|")
	for _, want := range []string{
		"|b|received 0/3000000|replayed 0/3000000|timeline 1, not streaming|last replayed commit 2026-11-13T09:00:00.5Z (1h0m0s ago)|most caught up\n",
		"|c|received 0/3000000|replayed 0/3000000|timeline 1, not streaming|last replayed commit 2026-11-13T09:00:00.5Z (the clocks differ)|most caught up\n",
		"|d|received 0/3000000|replayed 0/3000000|timeline 1, not streaming|replayed no transaction since it started|most caught up\n",
	} {
		if !strings.Contains(columns, want) {
			t.Errorf("the positions\n%s\ndo not hold %q", pipe.String(), want)
		}
	}
}
