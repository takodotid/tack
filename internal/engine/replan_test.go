package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// The ledgers in testdata/ledgers/<release> are the SiteFailover objects
// that tack plan -o yaml of that release printed, against the fakes of
// internal/cli's golden tests: a run writes the same spec. A resume
// composes their steps again ([RFC#0003] Requirement 21), so each still
// resumes.
func TestResumeAcceptsLedgersOfEarlierReleases(t *testing.T) {
	files, err := filepath.Glob("testdata/ledgers/*/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no ledgers: %v", err)
	}
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			sf := loadLedger(t, file)
			env := &engine.Env{Driver: cnpg.Driver{}, Git: &testenv.Git{}}
			if _, err := env.CheckResume(context.Background(), sf); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

func loadLedger(t *testing.T, file string) *failoverv1.SiteFailover {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var sf failoverv1.SiteFailover
	if err := yaml.UnmarshalStrict(b, &sf); err != nil {
		t.Fatal(err)
	}
	sf.Status.State = failoverv1.RunStopped
	return &sf
}

// #35 row 11. Anyone who may create a ledger can plant a run, and tack
// resume ran its steps with the operator's credentials. Each ledger below
// is a run's own plan with one change; tack resume refuses it before it
// changes or writes anything ([RFC#0003] Requirement 21).
func TestResumeRefusesStepsTheDriverDoesNotPlan(t *testing.T) {
	secret := failoverv1.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: "db", Name: "pg-a-superuser"}
	step := func(sf *failoverv1.SiteFailover, id string) *failoverv1.Step {
		i := slices.IndexFunc(sf.Spec.Steps, func(s failoverv1.Step) bool { return s.ID == id })
		if i < 0 {
			t.Fatalf("no step %s", id)
		}
		return &sf.Spec.Steps[i]
	}
	for _, tt := range []struct {
		name   string
		mode   failoverv1.Mode
		change func(sf *failoverv1.SiteFailover)
		want   string
	}{
		{name: "another patch", change: func(sf *failoverv1.SiteFailover) {
			step(sf, "demote-a").Patch.Merge = `{"spec":{"instances":0}}`
		}, want: "step demote-a differs from the plan in [patch]"},
		{name: "another object", change: func(sf *failoverv1.SiteFailover) {
			step(sf, "demote-a").Patch.Object = secret
		}, want: "step demote-a differs from the plan in [patch]"},
		{name: "a capture of another object", change: func(sf *failoverv1.SiteFailover) {
			c := step(sf, "capture-token").Capture
			c.Object, c.JSONPath = secret, "{.data.password}"
		}, want: "step capture-token differs from the plan in [capture]"},
		{name: "a wait on another field", change: func(sf *failoverv1.SiteFailover) {
			step(sf, "wait-demoted-a").Wait.Conditions[0].JSONPath = "{.status.secret}"
		}, want: "step wait-demoted-a differs from the plan in [wait]"},
		{name: "an extra step", change: func(sf *failoverv1.SiteFailover) {
			extra := *step(sf, "demote-a").DeepCopy()
			extra.ID, extra.Phase = "patch-secret", failoverv1.PhaseRelease
			sf.Spec.Steps = append(sf.Spec.Steps, extra)
		}, want: "step patch-secret is not in the plan"},
		{name: "a step moved", change: func(sf *failoverv1.SiteFailover) {
			i := slices.IndexFunc(sf.Spec.Steps, func(s failoverv1.Step) bool { return s.ID == "promote-b" })
			p := sf.Spec.Steps[i]
			sf.Spec.Steps = slices.Delete(sf.Spec.Steps, i, i+1)
			j := slices.IndexFunc(sf.Spec.Steps, func(s failoverv1.Step) bool { return s.ID == "demote-a" })
			sf.Spec.Steps = slices.Insert(sf.Spec.Steps, j, p)
		}, want: "is promote-b, where the plan has demote-a"},
		{name: "a hold dropped", change: func(sf *failoverv1.SiteFailover) {
			i := slices.IndexFunc(sf.Spec.Steps, func(s failoverv1.Step) bool { return s.ID == "hold-b-cluster-pg-b" })
			sf.Spec.Steps = slices.Delete(sf.Spec.Steps, i, i+1)
		}, want: "where the plan has hold-b-cluster-pg-b"},
		{name: "a step of to made deferrable", change: func(sf *failoverv1.SiteFailover) {
			step(sf, "promote-b").Deferrable = true
		}, want: "step promote-b differs from the plan in [deferrable]"},
		{name: "a reconcile of another Kustomization", change: func(sf *failoverv1.SiteFailover) {
			r := step(sf, "reconcile-hold-b").Reconcile
			r.Kustomizations = append(r.Kustomizations, failoverv1.ObjectKey{Namespace: "flux-system", Name: "infra"})
		}, want: "step reconcile-hold-b differs from the plan in [reconcile]"},
		{name: "a Git value", change: func(sf *failoverv1.SiteFailover) {
			v := `"pg-a"`
			step(sf, "git-commit").Git.Changes[0].Value = &v
		}, want: "the Git step sets spec.replica.primary of Cluster db/pg-a at site a to another value than the driver"},
		{name: "an extra Git change", change: func(sf *failoverv1.SiteFailover) {
			g := step(sf, "git-commit").Git
			g.Changes = append(g.Changes, failoverv1.GitChange{Site: "a", Object: secret, Field: "data.password", File: "clusters/a/secret.yaml", Path: "data.password"})
		}, want: "the Git step changes data.password of Secret db/pg-a-superuser at site a, which the driver does not change"},
		{name: "a release of another object", mode: failoverv1.ModeSiteLoss, change: func(sf *failoverv1.SiteFailover) {
			step(sf, "release-b-cluster-pg-b").Release.Object = secret
		}, want: "step release-b-cluster-pg-b differs from the plan in [release]"},
		{name: "another to", change: func(sf *failoverv1.SiteFailover) {
			sf.Spec.To = "a"
			sf.Spec.From = "b"
		}, want: "the Git step sets spec.replica.primary of Cluster db/pg-a at site a to another value than the driver"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			mode := tt.mode
			if mode == "" {
				mode = failoverv1.ModePlanned
			}
			if mode == failoverv1.ModeSiteLoss {
				f.clients.SetDown("a", true)
			}
			sf := f.plan("b", mode)
			f.clients.SetDown("a", false)
			sf.Status.State = failoverv1.RunStopped
			sf.Status.Reason = failoverv1.ReasonStepFailed
			tt.change(sf)
			if err := f.env("b").Ledger.Create(context.Background(), sf); err != nil {
				t.Fatal(err)
			}
			if err := f.env("b").Ledger.UpdateStatus(context.Background(), sf); err != nil {
				t.Fatal(err)
			}
			stored := f.ledger("b")
			got, err := f.resume("b", engine.Options{})
			if !errors.Is(err, engine.ErrCannotResume) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resume: %v, want a refusal with %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "do not resume it") {
				t.Errorf("the refusal gives no way on: %v", err)
			}
			if len(f.flux.Calls) > 0 || len(f.git.Commits) > 0 {
				t.Errorf("the refused resume changed something: Flux %v, %d commits", f.flux.Calls, len(f.git.Commits))
			}
			if got.ResourceVersion != stored.ResourceVersion {
				t.Errorf("the ledger was written: state %s", got.Status.State)
			}
			if p := field(f.cluster("a"), "spec", "replica", "primary"); p != "pg-a" {
				t.Errorf("Cluster pg-a declares %v primary, want pg-a unchanged", p)
			}
		})
	}
}

// The run's own plan, unchanged, resumes: the refusals above each come
// from their own change.
func TestResumeAcceptsItsOwnPlan(t *testing.T) {
	for _, mode := range []failoverv1.Mode{failoverv1.ModePlanned, failoverv1.ModeSiteLoss} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t)
			if mode == failoverv1.ModeSiteLoss {
				f.clients.SetDown("a", true)
			}
			sf := f.plan("b", mode)
			sf.Status.State = failoverv1.RunStopped
			if _, err := f.env("b").CheckResume(context.Background(), sf); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// #35 row 50. Before v1.3.0, a site written as n in the Workload file was
// recorded as "false". The check of site names reads the file only: a
// ledger that names a site "false" still resumes, and a file that quotes
// the name as "false" still finds it.
func TestResumeAcceptsSiteNamedFalse(t *testing.T) {
	w, err := config.Parse([]byte(strings.Replace(workloadYAML, "{name: a,", `{name: "false",`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	clients := testenv.NewClients(map[string][]client.Object{"false": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	env := &engine.Env{Clients: clients, Driver: cnpg.Driver{}, Flux: testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"), Git: &testenv.Git{},
		ReachWaits: []time.Duration{time.Millisecond, time.Millisecond}}
	p, err := env.MakePlan(context.Background(), engine.Request{Run: "run-1", Workload: w.Name, Spec: &w.Spec,
		Request: driver.Request{From: "false", To: "b", Mode: failoverv1.ModePlanned}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	sf := p.SiteFailover
	sf.Status.State = failoverv1.RunStopped
	if _, err := env.CheckResume(context.Background(), sf); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if sf.Spec.Steps[0].ID != "hold-false-cluster-pg-a" {
		t.Errorf("first step %s", sf.Spec.Steps[0].ID)
	}
}
