package engine_test

import (
	"context"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// countingGit counts the calls of Resolve: each one fetches Git.
type countingGit struct {
	*testenv.Git
	resolves int
}

func (g *countingGit) Resolve(ctx context.Context, w *failoverv1.WorkloadSpec, changes []driver.FieldChange) ([]failoverv1.GitChange, error) {
	g.resolves++
	return g.Git.Resolve(ctx, w, changes)
}

// #35 row 12. A plan reads Git once, before the operator can confirm: an
// optional removal with a mapping needs no fetch of its own.
func TestPlanFetchesGitOnce(t *testing.T) {
	f := newFixture(t)
	g := &countingGit{Git: f.git}
	env := f.env("b")
	env.Git = g
	p, err := env.MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if g.resolves != 1 {
		t.Errorf("the plan resolved Git %d times, want 1", g.resolves)
	}
	removals := 0
	for _, s := range p.SiteFailover.Spec.Steps {
		if s.Git == nil {
			continue
		}
		for _, c := range s.Git.Changes {
			if c.Value == nil {
				removals++
			}
		}
	}
	if removals != 2 {
		t.Errorf("the Git step removes %d fields, want the token at both sites", removals)
	}
}
