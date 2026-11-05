package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
)

// checkPlan composes the run's plan again from its ledger, and refuses a
// run whose steps differ from it ([RFC#0003] Requirement 21). Anyone who
// can write a ledger could otherwise make tack resume run any patch with
// the operator's credentials (#35 row 11).
//
// The plan is composed from the Workload snapshot and the request in the
// spec, the driver's steps, objects and Git changes, which depend on them
// alone, and the two inputs that a plan reads from the sites and Git: the
// Kustomization of each Hold step, and the file and path of each Git
// change. Gitops checks those at the commit ([RFC#0006] Requirement 15).
// Descriptions are not compared: they do not act.
func (e *Env) checkPlan(sf *failoverv1.SiteFailover) error {
	want, err := e.replan(sf)
	if err != nil {
		return fmt.Errorf("%w: the steps of run %s are not what driver %s plans from the run's Workload: %v. %s",
			ErrCannotResume, sf.Name, sf.Spec.Workload.Spec.Driver, err, planWayOn)
	}
	got := sf.Spec.Steps
	for i := 0; i < len(got) || i < len(want); i++ {
		var diff string
		switch {
		case i >= len(got):
			diff = fmt.Sprintf("the ledger has %d steps, and lacks step %s", len(got), want[i].ID)
		case i >= len(want):
			diff = fmt.Sprintf("step %s is not in the plan", got[i].ID)
		case got[i].ID != want[i].ID:
			diff = fmt.Sprintf("step %d is %s, where the plan has %s", i+1, got[i].ID, want[i].ID)
		default:
			diff = stepDiff(got[i], want[i])
		}
		if diff != "" {
			return fmt.Errorf("%w: the steps of run %s are not what driver %s plans from the run's Workload: %s. %s",
				ErrCannotResume, sf.Name, sf.Spec.Workload.Spec.Driver, diff, planWayOn)
		}
	}
	return nil
}

// planWayOn is the way on of a run whose steps checkPlan refuses.
const planWayOn = "The ledger was written outside tack, or by a tack that plans differently: do not resume it. Check who may write sitefailovers (docs/operations.md#who-may-write-ledgers), then abandon the run (tack abandon) and plan again, or start a new run with --take-over"

// replan composes the run's plan from its ledger, as MakePlan composed it.
func (e *Env) replan(sf *failoverv1.SiteFailover) ([]failoverv1.Step, error) {
	w := sf.Spec.Workload.Spec.DeepCopy()
	if err := e.Driver.Validate(w); err != nil {
		return nil, err
	}
	req := driver.Request{From: sf.Spec.From, To: sf.Spec.To, Mode: sf.Spec.Mode}
	if err := checkRequest(Request{Workload: sf.Spec.Workload.Name, Spec: w, Request: req}); err != nil {
		return nil, err
	}
	in := Inputs{
		Request:      Request{Run: sf.Name, Workload: sf.Spec.Workload.Name, Spec: w, Request: req},
		Topology:     &driver.Topology{},
		Owners:       map[string]map[failoverv1.ObjectRef]failoverv1.ObjectKey{},
		Fingerprints: sf.Spec.Fingerprints,
	}
	for _, s := range w.Sites {
		in.Topology.Sites = append(in.Topology.Sites, driver.SiteState{Name: s.Name, Objects: e.Driver.Objects(w, s.Name)})
	}
	// The Kustomization that applies each object, as the plan read it.
	for _, s := range sf.Spec.Steps {
		if s.Type != failoverv1.StepHold || s.Hold == nil || s.Hold.Kustomization == nil {
			continue
		}
		if in.Owners[s.Site] == nil {
			in.Owners[s.Site] = map[failoverv1.ObjectRef]failoverv1.ObjectKey{}
		}
		in.Owners[s.Site][s.Hold.Object] = *s.Hold.Kustomization
	}
	var err error
	if in.Failover, in.FollowUps, err = e.Driver.Steps(w, req); err != nil {
		return nil, err
	}
	if in.GitChanges, err = e.gitChanges(sf, w, in.Topology, req); err != nil {
		return nil, err
	}
	out, err := Compose(in)
	if err != nil {
		return nil, err
	}
	return out.Spec.Steps, nil
}

// gitChanges returns the Git step's changes when they are the driver's,
// as MakePlan keeps them: every change but an optional one without a
// mapping, with its value. Their file and path are the ledger's.
func (e *Env) gitChanges(sf *failoverv1.SiteFailover, w *failoverv1.WorkloadSpec, t *driver.Topology, req driver.Request) ([]failoverv1.GitChange, error) {
	changes, err := e.Driver.GitChange(w, t, req)
	if err != nil {
		return nil, err
	}
	var want []driver.FieldChange
	for _, c := range changes {
		if !c.Optional || e.Git.HasMapping(w, c) {
			want = append(want, c)
		}
	}
	i := slices.IndexFunc(sf.Spec.Steps, func(s failoverv1.Step) bool { return s.Type == failoverv1.StepGit })
	if i < 0 || sf.Spec.Steps[i].Git == nil {
		return nil, fmt.Errorf("the ledger has no Git step")
	}
	got := sf.Spec.Steps[i].Git.Changes
	for j := 0; j < len(got) || j < len(want); j++ {
		switch {
		case j >= len(got):
			return nil, fmt.Errorf("the Git step does not change %s", describe(want[j].Site, want[j].Object, want[j].Field))
		case j >= len(want):
			return nil, fmt.Errorf("the Git step changes %s, which the driver does not change", describe(got[j].Site, got[j].Object, got[j].Field))
		case got[j].Site != want[j].Site || got[j].Object != want[j].Object || got[j].Field != want[j].Field:
			return nil, fmt.Errorf("the Git step changes %s, where the driver changes %s",
				describe(got[j].Site, got[j].Object, got[j].Field), describe(want[j].Site, want[j].Object, want[j].Field))
		case !reflect.DeepEqual(got[j].Value, want[j].Value):
			return nil, fmt.Errorf("the Git step sets %s to another value than the driver", describe(got[j].Site, got[j].Object, got[j].Field))
		}
	}
	return got, nil
}

func describe(site string, obj failoverv1.ObjectRef, field string) string {
	return fmt.Sprintf("%s of %s at site %s", field, refString(obj), site)
}

// stepDiff names the parts of a step that differ from the plan's, its
// description left out.
func stepDiff(got, want failoverv1.Step) string {
	got.Description, want.Description = "", ""
	a, aerr := fields(got)
	b, berr := fields(want)
	if aerr != nil || berr != nil {
		return fmt.Sprintf("step %s cannot be compared", got.ID)
	}
	var keys []string
	for k := range a {
		if !reflect.DeepEqual(a[k], b[k]) {
			keys = append(keys, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return fmt.Sprintf("step %s differs from the plan in %v", got.ID, keys)
}

// fields returns a step as the API server stores it, by top-level key. An
// empty list or object reads as absent: the API server may drop either.
func fields(s failoverv1.Step) (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range m {
		if v = normal(v); v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m, nil
}

// normal returns v with each empty list, empty object and null removed.
func normal(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if c = normal(c); c == nil {
				delete(x, k)
			} else {
				x[k] = c
			}
		}
		if len(x) == 0 {
			return nil
		}
	case []any:
		if len(x) == 0 {
			return nil
		}
		for i := range x {
			x[i] = normal(x[i])
		}
	}
	return v
}
