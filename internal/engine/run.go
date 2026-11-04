package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/fault"
	"github.com/takodotid/tack/internal/kube"
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

// Start creates the ledger and runs the plan. With opts.TakeOver, the
// ledger's spec records the run it takes over, and that run's ledger is
// marked superseded when it answers.
func (e *Env) Start(ctx context.Context, sf *failoverv1.SiteFailover, opts Options) error {
	if opts.TakeOver != "" {
		sf.Spec.TakeOver = opts.TakeOver
	}
	now := metav1.NewTime(e.now())
	sf.Status.State = failoverv1.RunRunning
	sf.Status.StartedAt = &now
	sf.Status.Steps = nil
	for _, s := range sf.Spec.Steps {
		sf.Status.Steps = append(sf.Status.Steps, failoverv1.StepStatus{ID: s.ID, State: failoverv1.StepPending})
	}
	status := sf.Status.DeepCopy()
	if err := e.Ledger.Create(ctx, sf); err != nil {
		return fmt.Errorf("create the ledger: %w", err)
	}
	fault.Point("create-ledger", "after")
	// The status subresource ignores status on create. The first write
	// outlives a cancelled run: without it the ledger has no follow-ups.
	sf.Status = *status
	if err := e.write(ctx, sf); err != nil {
		return err
	}
	e.printf("ledger %s/%s created\n", sf.Namespace, sf.Name)
	if sf.Spec.TakeOver != "" && e.Supersede != nil {
		if err := e.Supersede(ctx, sf.Spec.TakeOver, sf.Name); err != nil {
			e.printf("  note      run %s is not marked as taken over (%v); spec.takeOver of this run records it\n", sf.Spec.TakeOver, err)
		}
	}
	return e.resume(ctx, sf, opts)
}

// Resume runs the remaining steps of a ledger. It refuses a ledger that
// never recorded its start ([RFC#0003] Requirement 20), a run that another
// run took over, and a run older than another run of the workload: its
// plan may no longer match the sites ([RFC#0003] Requirement 18).
func (e *Env) Resume(ctx context.Context, sf *failoverv1.SiteFailover, opts Options) error {
	if sf.Status.State == "" {
		return fmt.Errorf("%w: run %s never recorded its start, so none of its steps ran and its follow-ups are not in its ledger. Plan again, and start a new run with --take-over %s",
			ErrCannotResume, sf.Name, sf.Name)
	}
	if sf.Status.State == failoverv1.RunSucceeded && !hasRunnableDeferred(sf) {
		e.printf("run %s already succeeded\n", sf.Name)
		return nil
	}
	if err := e.checkLatest(ctx, sf); err != nil {
		return err
	}
	return e.resume(ctx, sf, opts)
}

// ErrCannotResume is returned when a run cannot resume: it never recorded
// its start, or another run took it over or started after it.
var ErrCannotResume = errors.New("the run cannot resume")

// checkLatest refuses a run that another run took over or started after.
// It reads the runs at every site of the run's Workload snapshot that
// answers.
func (e *Env) checkLatest(ctx context.Context, sf *failoverv1.SiteFailover) error {
	if by := sf.Status.SupersededBy; by != "" {
		return fmt.Errorf("%w: run %s was taken over by run %s; see tack status %s", ErrCannotResume, sf.Name, by, by)
	}
	if e.Runs == nil {
		return nil
	}
	runs, err := e.Runs(ctx, e.SiteChecks(sf.Spec.Mode, sf.Spec.To))
	if err != nil {
		return fmt.Errorf("search the workload's runs before resuming %s: %w", sf.Name, err)
	}
	for _, r := range runs {
		switch {
		case r.Name == sf.Name:
		case r.Spec.TakeOver == sf.Name:
			return fmt.Errorf("%w: run %s was taken over by run %s; see tack status %s", ErrCannotResume, sf.Name, r.Name, r.Name)
		case r.CreationTimestamp.After(sf.CreationTimestamp.Time):
			return fmt.Errorf("%w: run %s started after run %s, so the plan of %s may no longer match the sites; see tack status %s",
				ErrCannotResume, r.Name, sf.Name, sf.Name, r.Name)
		}
	}
	return nil
}

func (e *Env) resume(ctx context.Context, sf *failoverv1.SiteFailover, opts Options) error {
	if opts.AcceptGitHead != "" {
		// Before the run's commit, the accepted head is the commit's base.
		// After it, the Release phase checks Git against it.
		if sf.Status.Git.Commit == "" {
			sf.Status.Git.AcceptedRevision = opts.AcceptGitHead
		} else {
			sf.Status.Git.ReleaseRevision = opts.AcceptGitHead
		}
	}
	sf.Status.State, sf.Status.Reason, sf.Status.Message = failoverv1.RunRunning, "", ""
	if err := e.write(ctx, sf); err != nil {
		return err
	}
	r := &runner{Env: e, sf: sf, opts: opts, checks: e.SiteChecks(sf.Spec.Mode, sf.Spec.To),
		reachable: map[string]error{}, retried: map[string]string{}, gated: map[string]bool{}, identified: map[string]bool{}}
	return r.run(ctx)
}

type runner struct {
	*Env
	sf   *failoverv1.SiteFailover
	opts Options
	// checks are the checks of each site before the run's first step there.
	checks    *SiteChecks
	reachable map[string]error
	// retried notes, per site, a check that failed before the site
	// answered. The site's next step records it in its message.
	retried    map[string]string
	gated      map[string]bool
	identified map[string]bool
}

func (r *runner) run(ctx context.Context) error {
	for i := range r.sf.Spec.Steps {
		step := &r.sf.Spec.Steps[i]
		st := r.stepStatus(step.ID)
		if st.State == failoverv1.StepSucceeded {
			continue
		}
		if step.Site != "" {
			if err := r.siteReachable(ctx, step); err != nil {
				if ctx.Err() != nil {
					return r.end(ctx, step, ctx.Err())
				}
				if !kube.IsUnreachable(err) {
					// The site answered, or could not be checked: it is not
					// lost, and its step neither runs nor waits for it.
					return r.stopBefore(ctx, step, stop.Ask(failoverv1.ReasonStepFailed, "site %s: %v", step.Site, err))
				}
				if !step.Deferrable {
					return r.end(ctx, step, stop.Ask(failoverv1.ReasonStepFailed, "site %s is unreachable: %v", step.Site, err))
				}
				if st.State != failoverv1.StepDeferred {
					st.State, st.Message = failoverv1.StepDeferred, fmt.Sprintf("site %s unreachable: %v", step.Site, err)
					if err := r.write(ctx, r.sf); err != nil {
						return err
					}
					r.printf("  deferred  %s (site %s unreachable)\n", step.ID, step.Site)
				}
				continue
			}
			if err := r.identify(ctx, step.Site); err != nil {
				return r.stopBefore(ctx, step, err)
			}
			if step.Deferrable {
				if err := r.gate(ctx, step.Site); err != nil {
					if !errors.Is(err, errUnread) {
						return r.stopBefore(ctx, step, err)
					}
					// The site gave no answer to the read: it stays
					// unreachable for this process.
					r.reachable[step.Site] = &kube.UnreachableError{Err: err}
					st.State, st.Message = failoverv1.StepDeferred, fmt.Sprintf("site %s unreachable: %v", step.Site, err)
					if err := r.write(ctx, r.sf); err != nil {
						return err
					}
					r.printf("  deferred  %s (site %s did not answer the read before its steps)\n", step.ID, step.Site)
					continue
				}
			}
		}
		if err := r.runStep(ctx, step); err != nil {
			return err
		}
	}
	return r.finish(ctx)
}

// errUnread is returned by gate when the site did not answer its read.
var errUnread = errors.New("the site did not answer the read before its steps")

// gate reads a site whose steps are deferrable before the first of them
// that runs in this process, whatever that step's state: an approval to
// fence lasts one process, and the ledger does not tell a site that came
// back from one that never left. It stops the run when the site answers as
// primary, unless the operator approved fencing it. A site that does not
// answer the read is not known to be fenced: gate returns errUnread, and
// its steps are deferred.
//
// The read checks the site up to 3 times, in every mode: the site has just
// answered the check before its step, so a failed check is a blip, not a
// lost site. The read's other sites get one check; gate does not use them.
func (r *runner) gate(ctx context.Context, site string) error {
	if r.gated[site] {
		return nil
	}
	checks := r.SiteChecks(r.sf.Spec.Mode, r.sf.Spec.To)
	checks.Once = func(s string) bool { return s != site }
	topo, err := r.Driver.Discover(ctx, &r.sf.Spec.Workload.Spec, checks)
	if note := checks.Note(site); note != "" {
		r.note(site, "the read before its steps: "+note)
	}
	if err != nil {
		return err
	}
	s, ok := topo.Site(site)
	if !ok {
		return fmt.Errorf("site %s is not in the topology", site)
	}
	if !s.Reachable {
		return fmt.Errorf("%w: %s", errUnread, s.Error)
	}
	if s.Role == driver.RolePrimary && site != r.sf.Spec.To && !slices.Contains(r.opts.Fence, site) {
		return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
			"site %s answers as primary (%s); it should be fenced. Check it, then resume with --fence %s", site, s.Summary, site)
	}
	r.gated[site] = true
	return nil
}

// identify checks, once per process, that the site's context reaches the
// cluster the run recorded for the site. A site the run has no record of,
// such as one lost at plan time, is recorded on its first read.
func (r *runner) identify(ctx context.Context, site string) error {
	if r.identified[site] {
		return nil
	}
	live, err := r.fingerprint(ctx, site)
	if err != nil {
		return fmt.Errorf("site %s: %w", site, err)
	}
	want, ok := Fingerprint(r.sf, site)
	switch {
	case !ok:
		r.sf.Status.Fingerprints = append(r.sf.Status.Fingerprints, failoverv1.SiteFingerprint{Site: site, KubeSystemUID: live})
	case want != live:
		context := ""
		for _, s := range r.sf.Spec.Workload.Spec.Sites {
			if s.Name == site {
				context = s.KubeContext
			}
		}
		return stop.Ask(failoverv1.ReasonSiteMismatch,
			"context %q of site %s reaches another cluster than the run recorded: its kube-system namespace has UID %s, the ledger has %s. Point the context at the site's cluster, then resume. If the site's cluster was rebuilt, start a new run with --take-over %s",
			context, site, live, want, r.sf.Name)
	}
	r.identified[site] = true
	return nil
}

// runStep runs one step. Every ledger write replaces the status, so the
// step's status is looked up again after each write.
func (r *runner) runStep(ctx context.Context, step *failoverv1.Step) error {
	now := metav1.NewTime(r.now())
	r.startPhase(step.Phase, now)
	st := r.stepStatus(step.ID)
	st.State = failoverv1.StepRunning
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
		return r.end(ctx, step, err)
	}
	if note := r.retried[step.Site]; note != "" {
		delete(r.retried, step.Site)
		if msg != "" {
			msg += "; "
		}
		msg += note
	}
	done := metav1.NewTime(r.now())
	st = r.stepStatus(step.ID)
	st.State, st.FinishedAt, st.Message = failoverv1.StepSucceeded, &done, msg
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

// end records why the run cannot go on at a step.
func (r *runner) end(ctx context.Context, step *failoverv1.Step, err error) error {
	return r.halt(ctx, step, err, true)
}

// stopBefore records why the run cannot go on before a step starts, such as
// a stop by the read before its site's steps. The step keeps its state.
func (r *runner) stopBefore(ctx context.Context, step *failoverv1.Step, err error) error {
	return r.halt(ctx, step, err, false)
}

func (r *runner) halt(ctx context.Context, step *failoverv1.Step, err error, setState bool) error {
	se, ok := stop.As(err)
	if !ok {
		se = &stop.Error{Reason: failoverv1.ReasonStepFailed, Message: err.Error()}
	}
	st := r.stepStatus(step.ID)
	st.Message = se.Message
	if note := r.retried[step.Site]; note != "" {
		delete(r.retried, step.Site)
		st.Message += "; " + note
	}
	ret := ErrStopped
	state := failoverv1.StepFailed
	if se.Wait {
		state = failoverv1.StepPending
		r.sf.Status.State = failoverv1.RunWaiting
		ret = ErrWaiting
	} else {
		r.sf.Status.State = failoverv1.RunStopped
	}
	if setState {
		st.State = state
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
		if st.State == failoverv1.StepDeferred {
			deferred++
		}
	}
	now := metav1.NewTime(r.now())
	r.sf.Status.State = failoverv1.RunSucceeded
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

// siteReachable checks the step's site once per process. The site counts
// as unreachable only when every check fails, so a short network blip
// neither stops nor defers a step. A deferrable step of a SiteLoss run has
// one check: the run does not wait for a site it can do without.
func (r *runner) siteReachable(ctx context.Context, step *failoverv1.Step) error {
	site := step.Site
	if err, ok := r.reachable[site]; ok {
		return err
	}
	err := r.checks.Check(ctx, site, step.Deferrable && r.sf.Spec.Mode == failoverv1.ModeSiteLoss)
	if ctx.Err() != nil {
		return err
	}
	if note := r.checks.Note(site); note != "" {
		r.note(site, note)
	}
	r.reachable[site] = err
	return err
}

// note adds to what the site's next step records in its message.
func (r *runner) note(site, note string) {
	if r.retried[site] != "" {
		note = r.retried[site] + "; " + note
	}
	r.retried[site] = note
}

func (r *runner) stepStatus(id string) *failoverv1.StepStatus {
	for i := range r.sf.Status.Steps {
		if r.sf.Status.Steps[i].ID == id {
			return &r.sf.Status.Steps[i]
		}
	}
	r.sf.Status.Steps = append(r.sf.Status.Steps, failoverv1.StepStatus{ID: id, State: failoverv1.StepPending})
	return &r.sf.Status.Steps[len(r.sf.Status.Steps)-1]
}

func (r *runner) startPhase(p failoverv1.Phase, now metav1.Time) {
	r.sf.Status.Phase = p
	for i := range r.sf.Status.Phases {
		if r.sf.Status.Phases[i].Name == p {
			return
		}
	}
	r.sf.Status.Phases = append(r.sf.Status.Phases, failoverv1.PhaseStatus{Name: p, StartedAt: &now})
}

// endPhase records a phase's end once its last step that can run has run.
func (r *runner) endPhase(p failoverv1.Phase, now metav1.Time) {
	for _, s := range r.sf.Spec.Steps {
		if s.Phase != p {
			continue
		}
		st := r.stepStatus(s.ID)
		if st.State != failoverv1.StepSucceeded && st.State != failoverv1.StepDeferred {
			return
		}
	}
	for i := range r.sf.Status.Phases {
		if r.sf.Status.Phases[i].Name == p && r.sf.Status.Phases[i].FinishedAt == nil {
			r.sf.Status.Phases[i].FinishedAt = &now
		}
	}
}

// write records the status. It outlives a cancelled run, so an interrupt
// is still recorded.
func (e *Env) write(ctx context.Context, sf *failoverv1.SiteFailover) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
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

func hasRunnableDeferred(sf *failoverv1.SiteFailover) bool {
	for _, st := range sf.Status.Steps {
		if st.State == failoverv1.StepDeferred {
			return true
		}
	}
	return false
}
