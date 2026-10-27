package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/driver"
)

// LabelWorkload carries the workload's name on its SiteFailover objects.
const LabelWorkload = "failover.tako.id/workload"

// GitCommitRef is the placeholder for the run's own commit.
const GitCommitRef = "$(git.commit)"

// Request is what the operator asked for.
type Request struct {
	// Run names the SiteFailover.
	Run string
	// Workload is the Workload's name.
	Workload string
	// Spec is the Workload's spec, defaulted and validated.
	Spec *v1alpha1.WorkloadSpec
	driver.Request
}

// Plan is a SiteFailover ready to run, and what was read to make it.
type Plan struct {
	SiteFailover *v1alpha1.SiteFailover
	Topology     *driver.Topology
	// Notes are facts the operator should see before approving.
	Notes []string
}

// Inputs is everything Compose needs. Reading the clusters and Git fills it.
type Inputs struct {
	Request
	Topology *driver.Topology
	// Owners maps site, then object, to the Kustomization that applies it.
	// Sites that were unreachable have no entry.
	Owners map[string]map[v1alpha1.ObjectRef]v1alpha1.ObjectKey
	// Failover are the driver's steps.
	Failover []v1alpha1.Step
	// FollowUps are the driver's follow-ups.
	FollowUps []v1alpha1.FollowUp
	// GitChanges are the resolved Git changes.
	GitChanges []v1alpha1.GitChange
}

// MakePlan reads the sites and Git, and composes the plan. It changes nothing.
func (e *Env) MakePlan(ctx context.Context, req Request) (*Plan, error) {
	if err := checkRequest(req); err != nil {
		return nil, err
	}
	if err := e.Driver.Validate(req.Spec); err != nil {
		return nil, err
	}
	topo, err := e.Driver.Discover(ctx, req.Spec, e.Clients)
	if err != nil {
		return nil, err
	}
	to, _ := topo.Site(req.To)
	if to == nil || !to.Reachable {
		return nil, fmt.Errorf("site %q must be reachable to become primary", req.To)
	}
	if req.Mode == v1alpha1.ModePlanned {
		if from, _ := topo.Site(req.From); from == nil || !from.Reachable {
			return nil, fmt.Errorf("site %q is unreachable: a planned switchover needs it; use --mode site-loss if the site is lost", req.From)
		}
	}
	in := Inputs{Request: req, Topology: topo, Owners: map[string]map[v1alpha1.ObjectRef]v1alpha1.ObjectKey{}}
	var notes []string
	for _, s := range topo.Sites {
		if !s.Reachable {
			notes = append(notes, fmt.Sprintf("%s; the steps at site %s wait for it to come back", s.Error, s.Name))
			continue
		}
		owners := map[v1alpha1.ObjectRef]v1alpha1.ObjectKey{}
		for _, obj := range s.Objects {
			o, err := e.Flux.Owner(ctx, s.Name, obj)
			if err != nil {
				return nil, fmt.Errorf("site %s: %s: %w", s.Name, refString(obj), err)
			}
			if o.Helm {
				return nil, fmt.Errorf("site %s: %s is applied by helm-controller; Tack holds only objects that a Flux Kustomization applies", s.Name, refString(obj))
			}
			if o.Kustomization == nil {
				return nil, fmt.Errorf("site %s: %s is not applied by a Flux Kustomization", s.Name, refString(obj))
			}
			owners[obj] = *o.Kustomization
		}
		in.Owners[s.Name] = owners
	}
	in.Failover, in.FollowUps, err = e.Driver.Plan(req.Spec, topo, req.Request)
	if err != nil {
		return nil, err
	}
	changes, err := e.Driver.GitChange(req.Spec, topo, req.Request)
	if err != nil {
		return nil, err
	}
	var required []driver.FieldChange
	for _, c := range changes {
		if !c.Optional {
			required = append(required, c)
			continue
		}
		if _, err := e.Git.Resolve(ctx, req.Spec, []driver.FieldChange{c}); errors.Is(err, ErrNoMapping) {
			notes = append(notes, fmt.Sprintf("Git: no mapping for %s of %s at site %s: Tack leaves it as Git has it; the check before release stops the run if Git sets it",
				c.Field, refString(c.Object), c.Site))
			continue
		}
		required = append(required, c)
	}
	in.GitChanges, err = e.Git.Resolve(ctx, req.Spec, required)
	if err != nil {
		if in.GitChanges == nil {
			return nil, err
		}
		notes = append(notes, fmt.Sprintf("Git: %v; the Git step waits for Git (break-glass)", err))
	}
	sf, err := Compose(in)
	if err != nil {
		return nil, err
	}
	return &Plan{SiteFailover: sf, Topology: topo, Notes: notes}, nil
}

func checkRequest(req Request) error {
	if req.From == req.To {
		return fmt.Errorf("from and to are the same site %q", req.To)
	}
	var names []string
	for _, s := range req.Spec.Sites {
		names = append(names, s.Name)
	}
	for _, n := range []string{req.From, req.To} {
		found := false
		for _, s := range names {
			found = found || s == n
		}
		if !found {
			return fmt.Errorf("site %q is not in workload %q (sites: %s)", n, req.Workload, strings.Join(names, ", "))
		}
	}
	switch req.Mode {
	case v1alpha1.ModePlanned, v1alpha1.ModeSiteLoss:
	default:
		return fmt.Errorf("unknown mode %q", req.Mode)
	}
	return nil
}

// Compose builds the SiteFailover from its inputs. It reads nothing, so the
// same inputs always give the same plan.
func Compose(in Inputs) (*v1alpha1.SiteFailover, error) {
	spec := in.Spec
	fluxTimeout := spec.Timeouts.Flux.DeepCopy()
	deferrable := func(site string) bool {
		if site == "" || site == in.To {
			return false
		}
		return !(in.Mode == v1alpha1.ModePlanned && site == in.From)
	}
	var steps []v1alpha1.Step
	add := func(s v1alpha1.Step) {
		s.Deferrable = deferrable(s.Site)
		steps = append(steps, s)
	}
	sites := in.Topology.Sites

	// Hold.
	for _, s := range sites {
		if len(s.Objects) == 0 {
			continue
		}
		owners := in.Owners[s.Name]
		for _, obj := range s.Objects {
			st := &v1alpha1.ObjectStep{Object: obj}
			if k, ok := owners[obj]; ok {
				k := k
				st.Kustomization = &k
			}
			add(v1alpha1.Step{
				ID: stepID("hold", s.Name, obj), Phase: v1alpha1.PhaseHold, Type: v1alpha1.StepHold, Site: s.Name,
				Description: fmt.Sprintf("hold %s at site %s: Flux stops applying it", refString(obj), s.Name),
				Hold:        st,
			})
		}
		add(v1alpha1.Step{
			ID: "reconcile-hold-" + s.Name, Phase: v1alpha1.PhaseHold, Type: v1alpha1.StepReconcile, Site: s.Name,
			Description: fmt.Sprintf("reconcile Flux at site %s, so no apply started before the hold is still in flight", s.Name),
			Reconcile:   &v1alpha1.ReconcileStep{Kustomizations: uniqueOwners(owners), Timeout: *fluxTimeout},
		})
	}

	// Failover.
	for _, s := range in.Failover {
		s.Phase = v1alpha1.PhaseFailover
		add(s)
	}

	// Promote to GitOps.
	if len(in.GitChanges) == 0 {
		return nil, fmt.Errorf("the driver returned no Git change")
	}
	add(v1alpha1.Step{
		ID: "git-commit", Phase: v1alpha1.PhasePromoteToGitOps, Type: v1alpha1.StepGit,
		Description: fmt.Sprintf("commit the live state to %s branch %s", spec.Git.URL, spec.Git.Branch),
		Git: &v1alpha1.GitStep{
			Changes: in.GitChanges,
			Message: fmt.Sprintf("tack: %s primary %s to %s (%s)\n\nRun %s, mode %s.", in.Workload, in.From, in.To, strings.ToLower(string(in.Mode)), in.Run, in.Mode),
		},
	})
	for _, s := range sites {
		if len(s.Objects) == 0 {
			continue
		}
		add(v1alpha1.Step{
			ID: "reconcile-git-" + s.Name, Phase: v1alpha1.PhasePromoteToGitOps, Type: v1alpha1.StepReconcile, Site: s.Name,
			Description: fmt.Sprintf("wait for Flux at site %s to apply the run's commit (held objects stay as they are)", s.Name),
			Reconcile:   &v1alpha1.ReconcileStep{Kustomizations: uniqueOwners(in.Owners[s.Name]), Revision: GitCommitRef, Timeout: *fluxTimeout},
		})
	}

	// Release.
	for _, s := range sites {
		if len(s.Objects) == 0 {
			continue
		}
		owners := in.Owners[s.Name]
		fields := siteFields(in.GitChanges, s.Name)
		add(v1alpha1.Step{
			ID: "verify-git-" + s.Name, Phase: v1alpha1.PhaseRelease, Type: v1alpha1.StepVerifyGit, Site: s.Name,
			Description: fmt.Sprintf("check at site %s that Git, as Flux renders it, equals live", s.Name),
			VerifyGit:   &v1alpha1.VerifyStep{Objects: s.Objects, Fields: fields},
		})
		for _, obj := range s.Objects {
			st := &v1alpha1.ObjectStep{Object: obj}
			if k, ok := owners[obj]; ok {
				k := k
				st.Kustomization = &k
			}
			add(v1alpha1.Step{
				ID: stepID("release", s.Name, obj), Phase: v1alpha1.PhaseRelease, Type: v1alpha1.StepRelease, Site: s.Name,
				Description: fmt.Sprintf("release %s at site %s: Flux applies it again", refString(obj), s.Name),
				Release:     st,
			})
		}
		add(v1alpha1.Step{
			ID: "reconcile-release-" + s.Name, Phase: v1alpha1.PhaseRelease, Type: v1alpha1.StepReconcile, Site: s.Name,
			Description: fmt.Sprintf("reconcile Flux at site %s with the released objects", s.Name),
			Reconcile:   &v1alpha1.ReconcileStep{Kustomizations: uniqueOwners(owners), Revision: GitCommitRef, Timeout: *fluxTimeout},
		})
		add(v1alpha1.Step{
			ID: "verify-noop-" + s.Name, Phase: v1alpha1.PhaseRelease, Type: v1alpha1.StepVerifyNoop, Site: s.Name,
			Description: fmt.Sprintf("check at site %s that Flux changed nothing", s.Name),
			VerifyNoop:  &v1alpha1.VerifyStep{Objects: s.Objects, Fields: fields},
		})
	}
	if spec.Routing != nil && spec.Routing.Strait != nil {
		expected := ""
		for _, m := range spec.Routing.Strait.Members {
			if m.Site == in.To {
				expected = m.Member
			}
		}
		if expected == "" {
			return nil, fmt.Errorf("spec.routing.strait.members has no member for site %q", in.To)
		}
		for _, svc := range spec.Routing.Strait.Services {
			add(v1alpha1.Step{
				ID: "routing-" + svc.Site, Phase: v1alpha1.PhaseRelease, Type: v1alpha1.StepRouting, Site: svc.Site,
				Description: fmt.Sprintf("wait for Strait at site %s to route %s/%s to member %s only", svc.Site, svc.Namespace, svc.Name, expected),
				Routing: &v1alpha1.RoutingStep{
					Service:  v1alpha1.ObjectKey{Namespace: svc.Namespace, Name: svc.Name},
					Expected: expected,
					Timeout:  *spec.Timeouts.Routing.DeepCopy(),
				},
			})
		}
	}

	seen := map[string]bool{}
	for _, s := range steps {
		if seen[s.ID] {
			return nil, fmt.Errorf("step ID %q is repeated", s.ID)
		}
		seen[s.ID] = true
	}

	sf := &v1alpha1.SiteFailover{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "SiteFailover"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Run,
			Namespace: spec.Ledger.Namespace,
			Labels:    map[string]string{LabelWorkload: in.Workload},
		},
		Spec: v1alpha1.SiteFailoverSpec{
			Workload: v1alpha1.WorkloadSnapshot{Name: in.Workload, Spec: *spec.DeepCopy()},
			Mode:     in.Mode,
			From:     in.From,
			To:       in.To,
			Steps:    steps,
		},
		Status: v1alpha1.SiteFailoverStatus{FollowUps: in.FollowUps},
	}
	return sf, nil
}

func stepID(verb, site string, obj v1alpha1.ObjectRef) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-%s-%s", verb, site, obj.Kind, obj.Name))
}

func refString(o v1alpha1.ObjectRef) string {
	return fmt.Sprintf("%s %s/%s", o.Kind, o.Namespace, o.Name)
}

func uniqueOwners(owners map[v1alpha1.ObjectRef]v1alpha1.ObjectKey) []v1alpha1.ObjectKey {
	set := map[v1alpha1.ObjectKey]bool{}
	for _, k := range owners {
		set[k] = true
	}
	var out []v1alpha1.ObjectKey
	for k := range set {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func siteFields(changes []v1alpha1.GitChange, site string) []v1alpha1.ObjectField {
	var out []v1alpha1.ObjectField
	for _, c := range changes {
		if c.Site == site {
			out = append(out, v1alpha1.ObjectField{Object: c.Object, Field: c.Field})
		}
	}
	return out
}
