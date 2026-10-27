package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/fault"
	"github.com/takodotid/tack/internal/stop"
)

// Options are the operator's decisions for one invocation of run or resume.
type Options struct {
	// Fence lists sites the operator approved to fence although they
	// answer as primary.
	Fence []string
	// AcceptGitHead is a commit the operator accepted as the new base after
	// Git changed during the run.
	AcceptGitHead string
	// TakeOver is a run whose holds this run may take over.
	TakeOver string
}

// ErrStopped is returned when the run stops for the operator.
var ErrStopped = errors.New("run stopped")

// ErrWaiting is returned when the run waits for something outside it.
var ErrWaiting = errors.New("run waiting")

// Start creates the ledger and runs the plan.
func (e *Env) Start(ctx context.Context, sf *v1alpha1.SiteFailover, opts Options) error {
	now := metav1.NewTime(e.now())
	sf.Status.State = v1alpha1.RunRunning
	sf.Status.StartedAt = &now
	sf.Status.Steps = nil
	for _, s := range sf.Spec.Steps {
		sf.Status.Steps = append(sf.Status.Steps, v1alpha1.StepStatus{ID: s.ID, State: v1alpha1.StepPending})
	}
	status := sf.Status.DeepCopy()
	if err := e.Ledger.Create(ctx, sf); err != nil {
		return fmt.Errorf("create the ledger: %w", err)
	}
	// The status subresource ignores status on create.
	sf.Status = *status
	if err := e.Ledger.UpdateStatus(ctx, sf); err != nil {
		return fmt.Errorf("write the ledger: %w", err)
	}
	e.printf("ledger %s/%s created\n", sf.Namespace, sf.Name)
	return e.Resume(ctx, sf, opts)
}

// Resume runs the remaining steps of a ledger.
func (e *Env) Resume(ctx context.Context, sf *v1alpha1.SiteFailover, opts Options) error {
	if sf.Status.State == v1alpha1.RunSucceeded && !hasRunnableDeferred(sf) {
		e.printf("run %s already succeeded\n", sf.Name)
		return nil
	}
	if opts.AcceptGitHead != "" {
		sf.Status.Git.AcceptedRevision = opts.AcceptGitHead
	}
	sf.Status.State, sf.Status.Reason, sf.Status.Message = v1alpha1.RunRunning, "", ""
	if err := e.write(ctx, sf); err != nil {
		return err
	}
	r := &runner{Env: e, sf: sf, opts: opts, reachable: map[string]error{}, gated: map[string]bool{}}
	return r.run(ctx)
}

type runner struct {
	*Env
	sf        *v1alpha1.SiteFailover
	opts      Options
	reachable map[string]error
	gated     map[string]bool
}

func (r *runner) run(ctx context.Context) error {
	for i := range r.sf.Spec.Steps {
		step := &r.sf.Spec.Steps[i]
		st := r.stepStatus(step.ID)
		if st.State == v1alpha1.StepSucceeded {
			continue
		}
		if step.Site != "" {
			if err := r.siteReachable(ctx, step.Site); err != nil {
				if !step.Deferrable {
					return r.end(ctx, step, st, stop.Ask(v1alpha1.ReasonStepFailed, "site %s is unreachable: %v", step.Site, err))
				}
				if st.State != v1alpha1.StepDeferred {
					st.State, st.Message = v1alpha1.StepDeferred, fmt.Sprintf("site %s unreachable", step.Site)
					if err := r.write(ctx, r.sf); err != nil {
						return err
					}
					r.printf("  deferred  %s (site %s unreachable)\n", step.ID, step.Site)
				}
				continue
			}
			if st.State == v1alpha1.StepDeferred {
				if err := r.gate(ctx, step.Site); err != nil {
					return r.end(ctx, step, st, err)
				}
			}
		}
		if err := r.runStep(ctx, step, st); err != nil {
			return err
		}
	}
	return r.finish(ctx)
}

// gate stops the run when a site whose steps were deferred comes back
// answering as primary, unless the operator approved fencing it.
func (r *runner) gate(ctx context.Context, site string) error {
	if r.gated[site] {
		return nil
	}
	topo, err := r.Driver.Discover(ctx, &r.sf.Spec.Workload.Spec, r.Clients)
	if err != nil {
		return err
	}
	s, ok := topo.Site(site)
	if !ok {
		return fmt.Errorf("site %s is not in the topology", site)
	}
	if s.Role == driver.RolePrimary && site != r.sf.Spec.To && !slices.Contains(r.opts.Fence, site) {
		return stop.Ask(v1alpha1.ReasonFencedSiteIsPrimary,
			"site %s came back and answers as primary (%s); it should be fenced. Check it, then resume with --fence %s", site, s.Summary, site)
	}
	r.gated[site] = true
	return nil
}

func (r *runner) runStep(ctx context.Context, step *v1alpha1.Step, st *v1alpha1.StepStatus) error {
	now := metav1.NewTime(r.now())
	r.startPhase(step.Phase, now)
	st.State = v1alpha1.StepRunning
	st.StartedAt = &now
	st.FinishedAt = nil
	st.Attempts++
	st.Message = ""
	if err := r.write(ctx, r.sf); err != nil {
		return err
	}
	r.printf("  start     %s: %s\n", step.ID, step.Description)
	fault.Point(step.ID, "before")
	msg, err := r.exec(ctx, step)
	fault.Point(step.ID, "after")
	if err != nil {
		return r.end(ctx, step, st, err)
	}
	done := metav1.NewTime(r.now())
	st.State, st.FinishedAt, st.Message = v1alpha1.StepSucceeded, &done, msg
	r.endPhase(step.Phase, done)
	if err := r.write(ctx, r.sf); err != nil {
		return err
	}
	if msg != "" {
		r.printf("  done      %s: %s\n", step.ID, msg)
	} else {
		r.printf("  done      %s\n", step.ID)
	}
	return nil
}

// end records why the run cannot go on.
func (r *runner) end(ctx context.Context, step *v1alpha1.Step, st *v1alpha1.StepStatus, err error) error {
	se, ok := stop.As(err)
	if !ok {
		se = &stop.Error{Reason: v1alpha1.ReasonStepFailed, Message: err.Error()}
	}
	st.Message = se.Message
	ret := ErrStopped
	if se.Wait {
		st.State = v1alpha1.StepPending
		r.sf.Status.State = v1alpha1.RunWaiting
		ret = ErrWaiting
	} else {
		st.State = v1alpha1.StepFailed
		r.sf.Status.State = v1alpha1.RunStopped
	}
	r.sf.Status.Reason = se.Reason
	r.sf.Status.Message = fmt.Sprintf("step %s: %s", step.ID, se.Message)
	if werr := r.write(ctx, r.sf); werr != nil {
		return errors.Join(werr, err)
	}
	r.printf("  %-9s %s: %s: %s\n", strings.ToLower(string(r.sf.Status.State)), step.ID, se.Reason, se.Message)
	return fmt.Errorf("%w: %s", ret, r.sf.Status.Message)
}

func (r *runner) finish(ctx context.Context) error {
	deferred := 0
	for _, st := range r.sf.Status.Steps {
		if st.State == v1alpha1.StepDeferred {
			deferred++
		}
	}
	now := metav1.NewTime(r.now())
	r.sf.Status.State = v1alpha1.RunSucceeded
	r.sf.Status.Reason = ""
	r.sf.Status.Message = "every step ran"
	if deferred > 0 {
		r.sf.Status.Message = fmt.Sprintf("every reachable step ran; %d steps wait for their site to come back", deferred)
	}
	if r.sf.Status.FinishedAt == nil {
		r.sf.Status.FinishedAt = &now
	}
	if err := r.write(ctx, r.sf); err != nil {
		return err
	}
	r.printf("run %s succeeded: %s\n", r.sf.Name, r.sf.Status.Message)
	return nil
}

func (r *runner) siteReachable(ctx context.Context, site string) error {
	if err, ok := r.reachable[site]; ok {
		return err
	}
	err := r.Clients.Reachable(ctx, site)
	r.reachable[site] = err
	return err
}

func (r *runner) stepStatus(id string) *v1alpha1.StepStatus {
	for i := range r.sf.Status.Steps {
		if r.sf.Status.Steps[i].ID == id {
			return &r.sf.Status.Steps[i]
		}
	}
	r.sf.Status.Steps = append(r.sf.Status.Steps, v1alpha1.StepStatus{ID: id, State: v1alpha1.StepPending})
	return &r.sf.Status.Steps[len(r.sf.Status.Steps)-1]
}

func (r *runner) startPhase(p v1alpha1.Phase, now metav1.Time) {
	r.sf.Status.Phase = p
	for i := range r.sf.Status.Phases {
		if r.sf.Status.Phases[i].Name == p {
			return
		}
	}
	r.sf.Status.Phases = append(r.sf.Status.Phases, v1alpha1.PhaseStatus{Name: p, StartedAt: &now})
}

// endPhase records a phase's end once its last step that can run has run.
func (r *runner) endPhase(p v1alpha1.Phase, now metav1.Time) {
	for _, s := range r.sf.Spec.Steps {
		if s.Phase != p {
			continue
		}
		st := r.stepStatus(s.ID)
		if st.State != v1alpha1.StepSucceeded && st.State != v1alpha1.StepDeferred {
			return
		}
	}
	for i := range r.sf.Status.Phases {
		if r.sf.Status.Phases[i].Name == p && r.sf.Status.Phases[i].FinishedAt == nil {
			r.sf.Status.Phases[i].FinishedAt = &now
		}
	}
}

func (e *Env) write(ctx context.Context, sf *v1alpha1.SiteFailover) error {
	if err := e.Ledger.UpdateStatus(ctx, sf); err != nil {
		return fmt.Errorf("write the ledger (another tack process may be running this run): %w", err)
	}
	return nil
}

func (e *Env) printf(format string, a ...any) {
	if e.Out != nil {
		fmt.Fprintf(e.Out, format, a...)
	}
}

func hasRunnableDeferred(sf *v1alpha1.SiteFailover) bool {
	for _, st := range sf.Status.Steps {
		if st.State == v1alpha1.StepDeferred {
			return true
		}
	}
	return false
}
