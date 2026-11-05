package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	if err := e.create(ctx, sf, opts.TakeOver); err != nil {
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

// MaxRunNames is how many names Start tries for a run: the plan's name,
// then the same name with -2 up to -9 appended.
const MaxRunNames = 9

// create creates the ledger. A run's name holds its plan's time to the
// second, so two runs of a workload planned in one second get one name.
// When the name is taken by a run that no longer blocks a new one (it
// succeeded, another run took it over, or this run takes it over), create
// tries the name with -2, -3 and so on appended ([RFC#0003] Requirement 7).
// Any other run with the name refuses the run, as the search for active
// runs does: it may be a second tack process starting the same switch.
func (e *Env) create(ctx context.Context, sf *failoverv1.SiteFailover, takeOver string) error {
	base := sf.Name
	for n := 1; ; n++ {
		err := e.Ledger.Create(ctx, sf)
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		other, gerr := e.Ledger.Get(ctx, sf.Name)
		if gerr != nil {
			return fmt.Errorf("the name %s is taken, and its ledger cannot be read: %w", sf.Name, gerr)
		}
		switch {
		case other.Status.State == failoverv1.RunSucceeded, other.Status.SupersededBy != "", other.Name == takeOver:
		case other.Status.State == "":
			return fmt.Errorf("the name %s is taken by a run that has not recorded its start: another tack process may be starting it. Nothing was changed; check tack status %s, then plan again",
				sf.Name, sf.Name)
		default:
			return fmt.Errorf("the name %s is taken by a run that is %s: another tack process may be running it. Nothing was changed; check tack status %s",
				sf.Name, other.Status.State, sf.Name)
		}
		if n == MaxRunNames {
			return fmt.Errorf("the names %s to %s-%d are all taken: %w", base, base, n, err)
		}
		name := fmt.Sprintf("%s-%d", base, n+1)
		e.printf("  note      the name %s is taken by a run that is %s; this run is named %s\n", sf.Name, orPending(other.Status.State), name)
		rename(sf, name)
	}
}

// rename names a run that has no ledger yet. The run's commit message names
// it too.
func rename(sf *failoverv1.SiteFailover, name string) {
	sf.Name = name
	for i := range sf.Spec.Steps {
		if g := sf.Spec.Steps[i].Git; g != nil {
			g.Message = commitMessage(sf.Spec.Workload.Name, sf.Spec.From, sf.Spec.To, sf.Spec.Mode, name)
		}
	}
}

func orPending(s failoverv1.RunState) string {
	if s == "" {
		return "not started"
	}
	return string(s)
}

// Resume runs the remaining steps of a ledger, once CheckResume let it.
func (e *Env) Resume(ctx context.Context, sf *failoverv1.SiteFailover, opts Options) error {
	res, err := e.CheckResume(ctx, sf)
	if err != nil {
		return err
	}
	return res.run(ctx, opts)
}

// A Resumption is a run that CheckResume let resume.
type Resumption struct {
	env *Env
	sf  *failoverv1.SiteFailover
	// checks are the site checks of the search for other runs. The search
	// after the confirmation reuses them, so it adds no check of a site
	// ([RFC#0007] Requirement 5).
	checks *SiteChecks
}

// CheckResume refuses a run that cannot resume: a ledger that never
// recorded its start ([RFC#0003] Requirement 20), a run that another run
// took over, and a run older than another run of the workload, whose plan
// may no longer match the sites (Requirement 18). tack resume calls it
// before it asks the operator, then Resumption.Resume after.
func (e *Env) CheckResume(ctx context.Context, sf *failoverv1.SiteFailover) (*Resumption, error) {
	if sf.Status.State == "" {
		return nil, fmt.Errorf("%w: run %s never recorded its start, so none of its steps ran and its follow-ups are not in its ledger. Plan again, and start a new run with --take-over %s",
			ErrCannotResume, sf.Name, sf.Name)
	}
	res := &Resumption{env: e, sf: sf, checks: e.SiteChecks(sf.Spec.Mode, sf.Spec.To)}
	if res.done() {
		return res, nil
	}
	if err := e.checkLatest(ctx, sf, res.checks); err != nil {
		return nil, err
	}
	return res, nil
}

// Resume searches the workload's runs again, since another run may have
// taken this one over while the operator confirmed (issue #27), then runs
// the remaining steps.
func (r *Resumption) Resume(ctx context.Context, opts Options) error {
	if !r.done() {
		if err := r.env.checkLatest(ctx, r.sf, r.checks); err != nil {
			return err
		}
	}
	return r.run(ctx, opts)
}

// done tells whether the run succeeded with no deferred step left to run.
func (r *Resumption) done() bool {
	return r.sf.Status.State == failoverv1.RunSucceeded && !hasRunnableDeferred(r.sf)
}

func (r *Resumption) run(ctx context.Context, opts Options) error {
	if r.done() {
		r.env.printf("run %s already succeeded\n", r.sf.Name)
		return nil
	}
	return r.env.resume(ctx, r.sf, opts)
}

// ErrCannotResume is returned when a run cannot resume: it never recorded
// its start, or another run took it over or started after it.
var ErrCannotResume = errors.New("the run cannot resume")

// checkLatest refuses a run that another run took over or started after.
// It reads the runs at every site of the run's Workload snapshot that
// answers checks.
func (e *Env) checkLatest(ctx context.Context, sf *failoverv1.SiteFailover, checks *SiteChecks) error {
	if by := sf.Status.SupersededBy; by != "" {
		return fmt.Errorf("%w: run %s was taken over by run %s; see tack status %s", ErrCannotResume, sf.Name, by, by)
	}
	if e.Runs == nil {
		return nil
	}
	runs, err := e.Runs(ctx, checks)
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
		reachable: map[string]error{}, retried: map[string]string{}, gated: map[string]bool{}, identified: map[string]bool{},
		verified: map[string]bool{}}
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
	// verified holds the sites whose VerifyGit succeeded in this process.
	verified map[string]bool
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
					r.deferStep(step, fmt.Sprintf("site %s unreachable: %v", step.Site, err))
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
					r.deferStep(step, fmt.Sprintf("site %s unreachable: %v", step.Site, err))
					if err := r.write(ctx, r.sf); err != nil {
						return err
					}
					r.printf("  deferred  %s (site %s did not answer the read before its steps)\n", step.ID, step.Site)
					continue
				}
			}
		}
		if step.ID == r.promotion() {
			if err := r.beforePromotion(ctx); err != nil {
				return r.stopBefore(ctx, step, err)
			}
		}
		if step.Type == failoverv1.StepRelease && !r.verified[step.Site] {
			// The check of Git against live ran in an earlier process,
			// maybe hours ago: run it again before the site's first
			// release ([RFC#0005] Requirement 8).
			if v := r.verifyStep(step.Site); v != nil {
				if err := r.runStep(ctx, v); err != nil {
					return err
				}
			}
		}
		if err := r.runStep(ctx, step); err != nil {
			return err
		}
	}
	return r.finish(ctx)
}

// verifyStep returns the VerifyGit step of a site.
func (r *runner) verifyStep(site string) *failoverv1.Step {
	for i := range r.sf.Spec.Steps {
		if s := &r.sf.Spec.Steps[i]; s.Type == failoverv1.StepVerifyGit && s.Site == site {
			return s
		}
	}
	return nil
}

// deferStep records a step Deferred, with the time, and ends its phase when
// that was its last step that could run ([RFC#0003] Requirements 4 and 12).
func (r *runner) deferStep(step *failoverv1.Step, msg string) {
	now := metav1.NewTime(r.now())
	st := r.stepStatus(step.ID)
	st.State, st.FinishedAt, st.Message = failoverv1.StepDeferred, &now, msg
	r.endPhase(step.Phase, now)
}

// errUnread is returned by gate when the site did not answer its read.
var errUnread = errors.New("the site did not answer the read before its steps")

// gate reads a site whose steps are deferrable before the first of them
// that runs in this process, whatever that step's state: an approval to
// fence lasts one process, and the ledger does not tell a site that came
// back from one that never left. It stops the run when the site answers as
// primary, or declares itself primary, unless the operator approved
// fencing it. A site that does not
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
	if what := asPrimary(*s); what != "" && site != r.sf.Spec.To && !slices.Contains(r.opts.Fence, site) {
		return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
			"site %s %s (%s); it should be fenced. Check it, then resume with --fence %s", site, what, s.Summary, site)
	}
	r.gated[site] = true
	return nil
}

// asPrimary says how a site that answered shows itself primary: it
// answers as primary, or it declares itself primary, which CloudNativePG
// promotes at once, before an instance reports it ([RFC#0004] Motivation).
// It returns "" for any other site. The read before a site's steps and the
// read before the promotion both use it.
func asPrimary(s driver.SiteState) string {
	switch {
	case s.Role == driver.RolePrimary:
		return "answers as primary"
	case s.DeclaredPrimary == s.Name:
		return "declares itself primary"
	}
	return ""
}

// promotion returns the ID of the step that promotes to: the run's first
// Failover step at to. A driver's steps before it act at other sites.
func (r *runner) promotion() string {
	for _, s := range r.sf.Spec.Steps {
		if s.Phase == failoverv1.PhaseFailover && s.Site == r.sf.Spec.To {
			return s.ID
		}
	}
	return ""
}

// fencesAfter tells whether the run has a Failover step at site after the
// promotion, such as the step that declares the new primary at a third
// site. Only such a step can fence a site that is primary at the promotion.
func (r *runner) fencesAfter(site string) bool {
	promotion, after := r.promotion(), false
	for _, s := range r.sf.Spec.Steps {
		if s.ID == promotion {
			after = true
			continue
		}
		if after && s.Phase == failoverv1.PhaseFailover && s.Site == site {
			return true
		}
	}
	return false
}

// beforePromotion reads every site but to before the step that promotes
// to, in each process, and stops the run when a site that answers is
// primary, or declares itself primary: promoting to would give two
// primaries ([RFC#0007] Requirement 9). The read before a site's steps
// (gate) cannot see it: it skips to, and does not run for a site whose
// steps are done, such as an old primary promoted again by hand after its
// demotion (issue #45).
//
// A site that gave no answer in this process gives none here, without
// another check; any other site gets the run's checks, so a site checked
// before in this process is not checked again. A site that gives no answer
// does not stop the run. --fence lets the run go on only for a site that
// the run fences after the promotion.
func (r *runner) beforePromotion(ctx context.Context) error {
	to := r.sf.Spec.To
	topo, err := r.Driver.Discover(ctx, &r.sf.Spec.Workload.Spec, runChecks{r: r, skip: to})
	if err != nil {
		return fmt.Errorf("the read before the promotion: %w", err)
	}
	for _, s := range topo.Sites {
		if s.Name == to || !s.Reachable {
			continue
		}
		what := asPrimary(s)
		if what == "" {
			continue
		}
		if !r.fencesAfter(s.Name) {
			return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
				"site %s %s (%s). Promoting %s now gives two primaries, and no step of this run is left to fence %s, so --fence cannot help. The workload was changed outside this run, such as by a recovery by hand, and the run's plan may be stale: see docs/runbooks/abandon-run.md",
				s.Name, what, s.Summary, to, s.Name)
		}
		if !slices.Contains(r.opts.Fence, s.Name) {
			return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
				"site %s %s (%s), and this run fences it only after it promotes %s. Check it, then resume with --fence %s: %s and %s are then both primary until the run fences %s",
				s.Name, what, s.Summary, to, s.Name, to, s.Name, s.Name)
		}
	}
	return nil
}

// runChecks are the kube.Clients of a read during a run. A site that gave
// no answer in this process, to a check or to the read before its steps,
// stays unreachable without another check. Any other site gets the run's
// checks, which keep their result for the process.
type runChecks struct {
	r *runner
	// skip is a site the read does not read: it reports no answer. Its
	// step then fails on its own error, as without the read.
	skip string
}

// errNotRead is the answer runChecks give for the site they skip.
var errNotRead = &kube.UnreachableError{Err: errors.New("not read")}

func (c runChecks) Client(site string) (client.Client, error) { return c.r.checks.Client(site) }

func (c runChecks) Reachable(ctx context.Context, site string) error {
	if site == c.skip {
		return errNotRead
	}
	if err := c.r.reachable[site]; err != nil && kube.IsUnreachable(err) {
		return err
	}
	return c.r.checks.Reachable(ctx, site)
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
	msg = r.hideCaptured(msg)
	if note := r.retried[step.Site]; note != "" {
		delete(r.retried, step.Site)
		if msg != "" {
			msg += "; "
		}
		msg += note
	}
	if step.Type == failoverv1.StepVerifyGit {
		r.verified[step.Site] = true
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
	se = &stop.Error{Reason: se.Reason, Message: r.hideCaptured(se.Message), Wait: se.Wait}
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
		if state == failoverv1.StepFailed {
			now := metav1.NewTime(r.now())
			st.FinishedAt = &now
		}
	}
	r.sf.Status.Reason = se.Reason
	r.sf.Status.Message = fmt.Sprintf("step %s: %s", step.ID, se.Message)
	if werr := r.write(ctx, r.sf); werr != nil {
		return errors.Join(werr, errors.New(r.sf.Status.Message))
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

// startPhase records a phase's start at its first step. A step of a phase
// that already finished, such as a deferred step whose site came back,
// makes it unfinished again until endPhase.
func (r *runner) startPhase(p failoverv1.Phase, now metav1.Time) {
	r.sf.Status.Phase = p
	for i := range r.sf.Status.Phases {
		if r.sf.Status.Phases[i].Name == p {
			r.sf.Status.Phases[i].FinishedAt = nil
			return
		}
	}
	r.sf.Status.Phases = append(r.sf.Status.Phases, failoverv1.PhaseStatus{Name: p, StartedAt: &now})
}

// endPhase records a phase's end once each of its steps is Succeeded or
// Deferred.
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
