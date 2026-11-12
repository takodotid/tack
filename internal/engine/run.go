package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/fault"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/redact"
	"github.com/takodotid/tack/internal/stop"
	"github.com/takodotid/tack/internal/version"
)

// Options are the operator's decisions for one invocation of run or resume.
type Options struct {
	// Fence lists sites the operator approved to fence although they
	// answer as primary.
	Fence []string
	// AcceptGitHead is a commit the operator accepted as the new base after
	// Git changed during the run.
	AcceptGitHead string
	// TakeOver lists the runs whose holds this run may take over, in the
	// order the operator named them.
	TakeOver []string
	// Yes is set when the operator skipped the confirmation with --yes.
	Yes bool
	// Force is set when the operator took over a run that a tack process
	// may still run, with --force ([RFC#0003] Requirement 28).
	Force bool
}

// ErrStopped is returned when the run stops for the operator.
var ErrStopped = errors.New("run stopped")

// ErrWaiting is returned when the run waits for something outside it.
var ErrWaiting = errors.New("run waiting")

// Start creates the ledger and runs the plan. With opts.TakeOver, the
// ledger's spec records the runs it takes over: the first in
// spec.takeOver, every one in spec.takeOvers when there is more than one.
// Each of those runs' ledgers is marked superseded when it answers.
func (e *Env) Start(ctx context.Context, sf *failoverv1.SiteFailover, opts Options) error {
	var takeOver []string
	for _, r := range opts.TakeOver {
		if r != "" && !slices.Contains(takeOver, r) {
			takeOver = append(takeOver, r)
		}
	}
	if len(takeOver) > 0 {
		sf.Spec.TakeOver = takeOver[0]
	}
	if len(takeOver) > 1 {
		sf.Spec.TakeOvers = takeOver
	}
	// Read who runs this process before the ledger exists, so the first
	// status write follows the create without another request.
	e.user(ctx)
	now := metav1.NewTime(e.now())
	sf.Status.State = failoverv1.RunRunning
	sf.Status.StartedAt = &now
	sf.Status.Steps = nil
	for _, s := range sf.Spec.Steps {
		sf.Status.Steps = append(sf.Status.Steps, failoverv1.StepStatus{ID: s.ID, State: failoverv1.StepPending})
	}
	status := sf.Status.DeepCopy()
	if err := e.create(ctx, sf); err != nil {
		return fmt.Errorf("create the ledger: %w", err)
	}
	fault.Point("create-ledger", "after")
	if err := e.checkStarted(ctx, sf, opts); err != nil {
		return err
	}
	// The process holds the run's Lease from its first status write until
	// it returns ([RFC#0003] Requirement 28).
	defer e.holdLease(ctx, sf)()
	// The status subresource ignores status on create. The first write
	// outlives a cancelled run: without it the ledger has no follow-ups.
	sf.Status = *status
	if err := e.record(ctx, sf, e.Entry(ctx, sf, failoverv1.HistoryState(failoverv1.RunRunning), opts.Flags())); err != nil {
		return err
	}
	e.printf("ledger %s/%s created\n", sf.Namespace, sf.Name)
	for _, old := range sf.TakenOver() {
		if e.Supersede == nil {
			break
		}
		if err := e.Supersede(ctx, old, sf.Name); err != nil {
			e.printf("  note      run %s is not marked as taken over (%v); the spec of this run records it\n", old, err)
		}
	}
	return e.resume(ctx, sf, opts, false)
}

// Ended tells whether run r no longer blocks a new run of its workload
// ([RFC#0003] Requirement 8): it succeeded, it was abandoned, or another
// run took it over, as its status or the spec of a run in runs says.
func Ended(r *failoverv1.SiteFailover, runs []failoverv1.SiteFailover) bool {
	if r.Status.State == failoverv1.RunSucceeded || r.Status.SupersededBy != "" || r.Status.Abandoned != nil {
		return true
	}
	for i := range runs {
		if o := &runs[i]; o.Name != r.Name && o.TakesOver(r.Name) {
			return true
		}
	}
	return false
}

// checkStarted searches the workload's runs again once the ledger exists,
// before the first status write, any step, and the mark on the run it takes
// over. The search before Start and the create are not one operation: two
// tack processes can both find no active run, then both create a ledger
// (#35 row 35). Each searches after its create, so at most one goes on.
// A run that finds another active run, or cannot search, is abandoned: it
// changed nothing, and blocks no later run.
func (e *Env) checkStarted(ctx context.Context, sf *failoverv1.SiteFailover, opts Options) error {
	if e.Runs == nil {
		return nil
	}
	checks := e.StartChecks
	if checks == nil {
		checks = e.SiteChecks(sf.Spec.Mode, sf.Spec.To)
	}
	runs, err := e.Runs(ctx, checks)
	reason := ""
	if err != nil {
		reason = fmt.Sprintf("the search for other runs after the ledger's creation failed: %v", err)
	}
	for i := range runs {
		r := &runs[i]
		if reason != "" || r.Name == sf.Name || sf.TakesOver(r.Name) || Ended(r, runs) {
			continue
		}
		reason = fmt.Sprintf("run %s of the workload (%s) was created at the same time", r.Name, orPending(r.Status.State))
	}
	if reason == "" {
		return nil
	}
	sf.Status = failoverv1.SiteFailoverStatus{Abandoned: &failoverv1.Abandonment{At: metav1.NewTime(e.now()), Reason: reason}}
	entry := e.Entry(ctx, sf, failoverv1.HistoryAbandoned, opts.Flags())
	entry.Message = reason
	if werr := e.record(ctx, sf, entry); werr != nil {
		return fmt.Errorf("%s. Run %s ran no step and changed nothing, but its ledger could not be marked abandoned (%v): take it over with --take-over %s",
			reason, sf.Name, werr, sf.Name)
	}
	return fmt.Errorf("%s. Run %s ran no step, changed nothing, and is abandoned: see tack status, then plan again", reason, sf.Name)
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
func (e *Env) create(ctx context.Context, sf *failoverv1.SiteFailover) error {
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
		case other.Status.State == failoverv1.RunSucceeded, other.Status.SupersededBy != "", other.Status.Abandoned != nil, sf.TakesOver(other.Name):
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
// recorded its start ([RFC#0003] Requirement 20), a ledger that a newer
// tack wrote last (Requirement 24), a run whose steps are not
// the plan its driver composes from its ledger (Requirement 21), a run
// that another run took over, and a run older than another run of the
// workload, whose plan may no longer match the sites (Requirement 18).
// tack resume calls it before it asks the operator, then
// Resumption.Resume after.
func (e *Env) CheckResume(ctx context.Context, sf *failoverv1.SiteFailover) (*Resumption, error) {
	if a := sf.Status.Abandoned; a != nil {
		return nil, fmt.Errorf("%w: run %s was abandoned at %s: %s", ErrCannotResume, sf.Name, a.At.UTC().Format(time.RFC3339), a.Reason)
	}
	if sf.Status.State == "" {
		return nil, fmt.Errorf("%w: run %s never recorded its start, so none of its steps ran and its follow-ups are not in its ledger. Plan again, and start a new run with --take-over %s",
			ErrCannotResume, sf.Name, sf.Name)
	}
	res := &Resumption{env: e, sf: sf, checks: e.SiteChecks(sf.Spec.Mode, sf.Spec.To)}
	if res.done() {
		return res, nil
	}
	if err := CheckWriter(sf); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotResume, err)
	}
	if err := e.checkLatest(ctx, sf, res.checks); err != nil {
		return nil, err
	}
	if err := e.checkPlan(sf); err != nil {
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
	return r.env.resume(ctx, r.sf, opts, true)
}

// AcceptHEAD is the value of --accept-git-head that names the head the
// run's GitChanged stop named.
const AcceptHEAD = "HEAD"

// AcceptedHead returns the commit that --accept-git-head head accepts
// for the run. When the run's GitChanged stop named one commit
// (status.git.changedRevision), head must name it, or be HEAD, which
// stands for it: the operator reviewed that commit, not whatever the
// branch holds later (#35 row 37). A stop that named none, and a ledger
// of an older Tack, accept head as given.
func AcceptedHead(sf *failoverv1.SiteFailover, head string) (string, error) {
	changed := sf.Status.Git.ChangedRevision
	switch {
	case head == "" || changed == "":
		return head, nil
	case head == AcceptHEAD || sameCommit(head, changed):
		return changed, nil
	}
	return "", fmt.Errorf("--accept-git-head %s is not the commit the run stopped on: the stop named %s. Review that commit, then resume with --accept-git-head %s. Nothing was changed",
		head, changed, changed)
}

// AcceptHead returns the commit that --accept-git-head head accepts for
// the run: AcceptedHead's, and for HEAD when the run's stop named no
// commit, the branch head, read now. The ledger records a commit, never
// HEAD, which names none (PR #49). It fails, changing nothing, when Git
// cannot be read for HEAD.
func (e *Env) AcceptHead(ctx context.Context, sf *failoverv1.SiteFailover, head string) (string, error) {
	h, err := AcceptedHead(sf, head)
	if err != nil || h != AcceptHEAD {
		return h, err
	}
	w := &sf.Spec.Workload.Spec
	commit, err := e.Git.BranchHead(ctx, w)
	if err == nil && commit == "" {
		err = errors.New("the branch has no head")
	}
	if err != nil {
		return "", fmt.Errorf("--accept-git-head HEAD stands for the head of branch %s, and Git cannot be read (%v). Nothing was changed: resume once Git answers, or name the commit", w.Git.Branch, err)
	}
	return commit, nil
}

// ErrCannotResume is returned when a run cannot resume: it never recorded
// its start, or another run took it over or started after it.
var ErrCannotResume = errors.New("the run cannot resume")

// checkLatest refuses a run that another run took over or started after.
// It reads the runs at every site of the run's Workload snapshot that
// answers checks. Creation times come from the API server of each ledger's
// site, to the second: a run created in the same second at the same site,
// or within ClockSkew at another site, counts as started after this one,
// unless this run took it over.
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
	for i := range runs {
		r := &runs[i]
		switch {
		case r.Name == sf.Name:
		case r.Status.State == "" && r.Status.Abandoned != nil:
			// It ran no step, so the sites are as this run's plan read them.
		case r.TakesOver(sf.Name):
			return fmt.Errorf("%w: run %s was taken over by run %s; see tack status %s", ErrCannotResume, sf.Name, r.Name, r.Name)
		case sf.TakesOver(r.Name):
			// This run took it over, so it is older.
		case !r.CreationTimestamp.Before(&sf.CreationTimestamp) && r.Spec.To == sf.Spec.To:
			return fmt.Errorf("%w: run %s started after run %s, or in the same second, so the plan of %s may no longer match the sites; see tack status %s",
				ErrCannotResume, r.Name, sf.Name, sf.Name, r.Name)
		case r.CreationTimestamp.After(sf.CreationTimestamp.Add(-ClockSkew)) && r.Spec.To != sf.Spec.To:
			return fmt.Errorf("%w: run %s started after run %s, or less than %s before it by the clock of another site's API server, so the plan of %s may no longer match the sites; see tack status %s",
				ErrCannotResume, r.Name, sf.Name, ClockSkew, sf.Name, r.Name)
		}
	}
	return nil
}

// ClockSkew is how far apart the clocks of two sites' API servers may be.
// A run's ledger lives at its to site, and that site's API server sets its
// creation time, to the second. A run of another site created less than
// ClockSkew before this one may have started after it (#38 item 7).
const ClockSkew = 10 * time.Second

// CertainlyAfter tells whether the clocks of the sites' API servers show
// ledger b created after ledger a: at the same site, in a later second; at
// another site, ClockSkew or more later. It is the complement of what
// checkLatest counts as a run started after another ([RFC#0003]
// Requirement 18): where the clocks cannot tell, neither ledger counts as
// created after the other.
func CertainlyAfter(b, a *failoverv1.SiteFailover, sameSite bool) bool {
	if sameSite {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return !b.CreationTimestamp.Time.Before(a.CreationTimestamp.Add(ClockSkew))
}

// resume runs the remaining steps. A resume, as opposed to the start
// that Start already recorded, appends its entry Running to the history.
func (e *Env) resume(ctx context.Context, sf *failoverv1.SiteFailover, opts Options, resumed bool) error {
	if resumed {
		defer e.holdLease(ctx, sf)()
	}
	head, err := e.AcceptHead(ctx, sf, opts.AcceptGitHead)
	if err != nil {
		return err
	}
	if head != "" {
		// Before the run's commit, the accepted head is the commit's base.
		// After it, the Release phase checks Git against it.
		if sf.Status.Git.Commit == "" {
			sf.Status.Git.AcceptedRevision = head
		} else {
			sf.Status.Git.ReleaseRevision = head
		}
	}
	sf.Status.Git.ChangedRevision = ""
	// A run that succeeded stays Succeeded until one of its deferred steps
	// starts: a process that dies before then, such as on a site that is
	// still down, must not leave a finished run Running, which blocks
	// every new run (#35 row 31).
	if sf.Status.State != failoverv1.RunSucceeded {
		sf.Status.State, sf.Status.Reason, sf.Status.Message = failoverv1.RunRunning, "", ""
		var entry *failoverv1.HistoryEntry
		if resumed {
			h := e.Entry(ctx, sf, failoverv1.HistoryState(failoverv1.RunRunning), opts.Flags())
			entry = &h
		}
		if err := e.writeEntry(ctx, sf, entry); err != nil {
			return err
		}
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
	// rechecked is set once the read before the first Failover step
	// passed in this process.
	rechecked bool
}

func (r *runner) run(ctx context.Context) error {
	defer r.checkDeferrable(ctx)()
	for i := range r.sf.Spec.Steps {
		step := &r.sf.Spec.Steps[i]
		st := r.stepStatus(step.ID)
		if st.State == failoverv1.StepSucceeded {
			continue
		}
		if step.Site != "" {
			if err := r.siteReachable(ctx, step); err != nil {
				if ctx.Err() != nil {
					// An interrupt during the check is no answer from the
					// site: the step did not start, and keeps its state.
					return r.stopBefore(ctx, step, ctx.Err())
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
					r.event(ctx, r.sf, true, "StepDeferred", step.ID+": "+r.stepStatus(step.ID).Message)
					r.printf("  deferred  %s (site %s unreachable)\n", step.ID, step.Site)
				}
				continue
			}
			if err := r.identify(ctx, step.Site); err != nil {
				if ctx.Err() != nil {
					return r.stopBefore(ctx, step, ctx.Err())
				}
				if !kube.IsUnreachable(err) || !step.Deferrable {
					return r.stopBefore(ctx, step, err)
				}
				// The site gave no answer to the read of its cluster's
				// identity: it stays unreachable for this process.
				r.reachable[step.Site] = err
				r.deferStep(step, fmt.Sprintf("site %s unreachable: %v", step.Site, err))
				if err := r.write(ctx, r.sf); err != nil {
					return err
				}
				r.event(ctx, r.sf, true, "StepDeferred", step.ID+": "+r.stepStatus(step.ID).Message)
				r.printf("  deferred  %s (site %s did not answer the read of its cluster's identity)\n", step.ID, step.Site)
				continue
			}
		}
		if step.Phase == failoverv1.PhaseFailover && !r.rechecked && !r.switchStarted() {
			if err := r.recheck(ctx); err != nil {
				return r.stopBefore(ctx, step, err)
			}
		}
		// The read of the other sites comes before the read before the
		// step's site, so a site that is primary again after its demotion
		// gets one way on, not first --fence and then a stop that --fence
		// cannot help.
		if r.readsOthers(i) {
			if err := r.readOthers(ctx, i); err != nil {
				return r.stopBefore(ctx, step, err)
			}
		}
		if step.Site != "" {
			if step.Deferrable {
				if err := r.gate(ctx, step.Site); err != nil {
					if ctx.Err() != nil {
						// An interrupt during the read is no answer from
						// the site: the run stops before the step, which
						// keeps its state (#35 row 34).
						return r.stopBefore(ctx, step, ctx.Err())
					}
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
					r.event(ctx, r.sf, true, "StepDeferred", step.ID+": "+r.stepStatus(step.ID).Message)
					r.printf("  deferred  %s (site %s did not answer the read before its steps)\n", step.ID, step.Site)
					continue
				}
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

// switchStarted tells whether a Failover step started in any process: it
// is neither Pending nor Deferred, or it has an attempt.
func (r *runner) switchStarted() bool {
	for _, s := range r.sf.Spec.Steps {
		if s.Phase != failoverv1.PhaseFailover {
			continue
		}
		st := r.stepStatus(s.ID)
		if st.Attempts > 0 || st.State != failoverv1.StepPending && st.State != failoverv1.StepDeferred {
			return true
		}
	}
	return false
}

// recheck reads every site again before the run's first Failover step,
// while none started, and refuses a run that its driver no longer allows
// on what it reads, as the read after the confirmation of tack run does
// ([RFC#0009] Requirement 13). A run can wait between Hold and its switch
// for hours, such as at a stop, and the operator may have switched the
// primary meanwhile (#46). A site whose steps can wait for it and that
// answers as primary, or declares itself primary, is left out: the read
// before its steps and the read of the other sites decide on it, with or
// without --fence ([RFC#0007] Requirements 6 and 9).
func (r *runner) recheck(ctx context.Context) error {
	topo, err := r.discover(ctx, &r.sf.Spec.Workload.Spec, runChecks{r: r})
	if err != nil {
		return fmt.Errorf("the read before the first Failover step: %w", err)
	}
	req := r.request()
	for i := range topo.Sites {
		if s := &topo.Sites[i]; s.Reachable && deferrable(req, s.Name) && asPrimary(*s) != "" {
			s.Reachable = false
		}
	}
	if _, _, err := r.Driver.Plan(&r.sf.Spec.Workload.Spec, topo, req); err != nil {
		return fmt.Errorf("the read before the first Failover step: driver %s no longer allows the run: %v. No Failover step of this run ran. When the sites are back as the plan read them, resume; else abandon the run (%s) and plan again",
			r.sf.Spec.Workload.Spec.Driver, err, version.DocLink("docs/runbooks/abandon-run.md"))
	}
	r.rechecked = true
	return nil
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
// lost site. Its reads are repeated as its checks are. It reads no other
// site: gate does not use them, and an error there stopped the run (#64).
func (r *runner) gate(ctx context.Context, site string) error {
	if r.gated[site] {
		return nil
	}
	checks := r.SiteChecks(r.sf.Spec.Mode, r.sf.Spec.To)
	checks.Once = nil
	topo, err := r.discover(ctx, &r.sf.Spec.Workload.Spec, oneSite{SiteChecks: checks, site: site})
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

// index returns the position of the step id in the plan, or -1.
func (r *runner) index(id string) int {
	if id == "" {
		return -1
	}
	return slices.IndexFunc(r.sf.Spec.Steps, func(s failoverv1.Step) bool { return s.ID == id })
}

// request is the run's request, as its driver takes it.
func (r *runner) request() driver.Request {
	return driver.Request{From: r.sf.Spec.From, To: r.sf.Spec.To, Mode: r.sf.Spec.Mode}
}

// fencesAfter tells whether the step that makes site declare another
// primary, as the driver names it, comes after the promotion and has not
// succeeded, such as the step that declares the new primary at a third
// site. Only such a step can fence a site that is primary at the promotion
// or after it.
func (r *runner) fencesAfter(site string) bool {
	p, d := r.index(r.promotion()), r.index(r.Driver.Demotion(r.request(), site))
	return p >= 0 && d > p && r.stepStatus(r.sf.Spec.Steps[d].ID).State != failoverv1.StepSucceeded
}

// readsOthers tells whether the run reads every site but to before step
// i: a Failover step after the step that demotes or fences from, the
// promotion among them ([RFC#0007] Requirement 9). A plan without the
// demotion the driver names reads only before the promotion.
func (r *runner) readsOthers(i int) bool {
	step := &r.sf.Spec.Steps[i]
	if step.Phase != failoverv1.PhaseFailover {
		return false
	}
	if step.ID == r.promotion() {
		return true
	}
	d := r.index(r.Driver.Demotion(r.request(), r.sf.Spec.From))
	return d >= 0 && i > d
}

// readOthers reads every site but to before step i, in each process, and
// stops the run when a site that answers is primary, or declares itself
// primary ([RFC#0007] Requirement 9). Before the promotion, promoting to
// would give two primaries; after it, there are two. The read before a
// site's steps (gate) cannot see it: it skips to, and does not run for a
// site whose steps are done, such as an old primary promoted again by hand
// after its demotion (issues #45 and #46). Before the promotion, it also
// compares the values the run captured with their source (checkCaptured).
//
// A site that gave no answer in this process gives none here, without
// another check; any other site gets the run's checks, so a site checked
// before in this process is not checked again. A site that gives no answer
// does not stop the run. --fence lets the run go on only for a site that
// the run fences after the promotion, with a step not yet done.
func (r *runner) readOthers(ctx context.Context, i int) error {
	to := r.sf.Spec.To
	p := r.index(r.promotion())
	promoted := p >= 0 && i > p
	read := "the read after the demotion"
	if i == p {
		read = "the read before the promotion"
	}
	topo, err := r.discover(ctx, &r.sf.Spec.Workload.Spec, runChecks{r: r, skip: to})
	if err != nil {
		return fmt.Errorf("%s: %w", read, err)
	}
	// fenced are the follow-ups of the sites --fence lets through, recorded
	// once no site stops the run.
	var fenced []failoverv1.FollowUp
	for _, s := range topo.Sites {
		if s.Name == to || !s.Reachable {
			continue
		}
		what := asPrimary(s)
		if what == "" {
			continue
		}
		fences, fence := r.fencesAfter(s.Name), slices.Contains(r.opts.Fence, s.Name)
		switch {
		case !fences && !promoted:
			return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
				"site %s %s (%s). Promoting %s gives two primaries, and no step of this run is left to fence %s, so --fence cannot help. The workload was changed outside this run, such as by a recovery by hand, and the run's plan may be stale: see %s",
				s.Name, what, s.Summary, to, s.Name, version.DocLink("docs/runbooks/abandon-run.md"))
		case !fences:
			return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
				"site %s %s (%s), and so does %s, which this run promoted: two primaries. No step of this run is left to fence %s, so --fence cannot help. The workload was changed outside this run, such as by a recovery by hand: see %s",
				s.Name, what, s.Summary, to, s.Name, version.DocLink("docs/runbooks/abandon-run.md"))
		case !fence && !promoted:
			return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
				"site %s %s (%s), and this run fences it only after it promotes %s. Check it, then resume with --fence %s: %s and %s are then both primary until the run fences %s",
				s.Name, what, s.Summary, to, s.Name, to, s.Name, s.Name)
		case !fence:
			return stop.Ask(failoverv1.ReasonFencedSiteIsPrimary,
				"site %s %s (%s), and so does %s, which this run promoted, until this run fences %s. Check it, then resume with --fence %s",
				s.Name, what, s.Summary, to, s.Name, s.Name)
		}
		// --fence lets the run promote to, or go on after it, while the
		// site is primary: both may take writes until the run fences it
		// (#46).
		fenced = append(fenced, failoverv1.FollowUp{Site: s.Name, Action: FollowUpRebuild,
			Reason: fmt.Sprintf("site %s %s while this run promoted %s, which the operator let go on with --fence %s, so %s may hold writes that %s lacks: recreate its member from %s (%s)",
				s.Name, what, to, s.Name, s.Name, to, to, version.DocLink("docs/runbooks/rebuild-old-primary.md"))})
	}
	if i == p {
		if err := r.checkCaptured(ctx, i); err != nil {
			return err
		}
	}
	for _, f := range fenced {
		r.rebuild(f)
	}
	return nil
}

// rebuild records a follow-up Rebuild, once per site. The ledger's next
// write carries it.
func (r *runner) rebuild(f failoverv1.FollowUp) {
	for _, o := range r.sf.Status.FollowUps {
		if o.Site == f.Site && o.Action == f.Action {
			return
		}
	}
	r.sf.Status.FollowUps = append(r.sf.Status.FollowUps, f)
	r.printf("  note      follow-up for site %s: %s (%s)\n", f.Site, f.Action, f.Reason)
}

// checkCaptured reads again, before the promotion, each value a Capture
// step before step i captured, and stops the run with TokenMissing when
// the value at its source changed: the workload was changed outside the
// run, such as an old primary promoted again and demoted again by hand,
// and the promotion would use the token of a demotion that a later one
// replaced (#46). A site that gave no answer in this process is not read:
// the run then promotes with the value it captured, as with from down
// (docs/runbooks/no-primary-after-demotion.md, option 1). Neither value is
// printed.
func (r *runner) checkCaptured(ctx context.Context, i int) error {
	for _, s := range r.sf.Spec.Steps[:i] {
		c := s.Capture
		if s.Type != failoverv1.StepCapture || c == nil || r.stepStatus(s.ID).State != failoverv1.StepSucceeded {
			continue
		}
		want := r.sf.Status.Captured[c.Into]
		if want == "" {
			// The step that uses it stops with TokenMissing.
			continue
		}
		if err := (runChecks{r: r}).Reachable(ctx, s.Site); err != nil {
			if !kube.IsUnreachable(err) {
				return fmt.Errorf("the read before the promotion: site %s: %w", s.Site, err)
			}
			r.note(r.sf.Spec.To, fmt.Sprintf("site %s gave no answer, so %s was not compared with %s", s.Site, c.Into, refString(c.Object)))
			r.printf("  note      site %s gave no answer: %s is not compared with %s\n", s.Site, c.Into, refString(c.Object))
			continue
		}
		cl, err := r.checks.Client(s.Site)
		if err != nil {
			return fmt.Errorf("the read before the promotion: site %s: %w", s.Site, err)
		}
		u := newObject(c.Object)
		if err := cl.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
			return fmt.Errorf("the read before the promotion: site %s: read %s: %w", s.Site, refString(c.Object), err)
		}
		got, err := JSONPath(u.Object, c.JSONPath)
		if err != nil {
			return fmt.Errorf("the read before the promotion: site %s: %w", s.Site, err)
		}
		if got != want {
			now := "another value"
			if got == "" {
				now = "no value"
			}
			return stop.Ask(failoverv1.ReasonTokenMissing,
				"%s now has %s at %s, not the value step %s captured as %s: the workload was changed outside this run, such as a demotion by hand after a promotion by hand, and the run must not promote with %s. The run's plan may be stale: see %s",
				refString(c.Object), now, c.JSONPath, s.ID, c.Into, c.Into, version.DocLink("docs/runbooks/abandon-run.md"))
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

// ahead checks the sites at once through c ([RFC#0007] Requirement 10):
// the site c skips and a site that gave no answer in this process get no
// check, as in the read.
func (c runChecks) ahead(ctx context.Context, sites []string) func() {
	var checked []string
	for _, site := range sites {
		if err := c.r.reachable[site]; site != c.skip && (err == nil || !kube.IsUnreachable(err)) {
			checked = append(checked, site)
		}
	}
	c.r.checks.credentials(ctx, checked)
	return c.r.checks.checkAhead(ctx, sites, c.Reachable)
}

// oneSite are the kube.Clients of a read of one site, the read before a
// site's steps: every other site reports no answer, with no check and no
// read.
type oneSite struct {
	*SiteChecks
	site string
}

func (c oneSite) Reachable(ctx context.Context, site string) error {
	if site != c.site {
		return errNotRead
	}
	return c.SiteChecks.Reachable(ctx, site)
}

// ahead checks nothing at once: the read checks its one site itself.
func (c oneSite) ahead(context.Context, []string) func() { return func() {} }

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
	var entry *failoverv1.HistoryEntry
	if r.sf.Status.State == failoverv1.RunSucceeded {
		r.sf.Status.State, r.sf.Status.Reason, r.sf.Status.Message = failoverv1.RunRunning, "", ""
		h := r.Entry(ctx, r.sf, failoverv1.HistoryState(failoverv1.RunRunning), r.opts.Flags())
		entry = &h
	}
	if err := r.writeEntry(ctx, r.sf, entry); err != nil {
		return err
	}
	r.event(ctx, r.sf, false, "StepStarted", step.ID+": "+step.Description)
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
	r.event(ctx, r.sf, false, "StepSucceeded", joinMessage(step.ID, msg))
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
	se = &stop.Error{Reason: se.Reason, Message: r.hideCaptured(se.Message), Wait: se.Wait, Head: se.Head}
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
	r.sf.Status.Git.ChangedRevision = ""
	if se.Reason == failoverv1.ReasonGitChanged {
		r.sf.Status.Git.ChangedRevision = se.Head
	}
	if werr := r.record(ctx, r.sf, r.Entry(ctx, r.sf, failoverv1.HistoryState(r.sf.Status.State), r.opts.Flags())); werr != nil {
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
	// A ledger of v1.2.0 ended a phase only after a step of it succeeded,
	// so a phase whose last steps were deferred stayed unfinished (PR #42).
	for _, p := range r.sf.Status.Phases {
		r.endPhase(p.Name, r.lastFinish(p.Name, now))
	}
	r.sf.Status.State = failoverv1.RunSucceeded
	r.sf.Status.Reason = ""
	r.sf.Status.Message = "every step ran"
	if deferred > 0 {
		r.sf.Status.Message = fmt.Sprintf("every reachable step ran; %d steps wait for their site to come back", deferred)
	}
	if r.sf.Status.FinishedAt == nil {
		r.sf.Status.FinishedAt = &now
	}
	if err := r.record(ctx, r.sf, r.Entry(ctx, r.sf, failoverv1.HistoryState(failoverv1.RunSucceeded), r.opts.Flags())); err != nil {
		return err
	}
	r.printf("run %s succeeded: %s\n", r.sf.Name, r.sf.Status.Message)
	return nil
}

// checkDeferrable starts at once the checks before the first step of each
// site whose steps are deferrable and not all Succeeded ([RFC#0007]
// Requirement 5), so that sites that give no answer wait together, not
// one after another (#58 row 4). Each site gets the checks it would get
// before its first step, no more, and that step waits for them
// (SiteChecks.Check holds the site). The end stops the checks still
// running: a check stopped so does not count.
func (r *runner) checkDeferrable(ctx context.Context) (end func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	started := map[string]bool{}
	for i := range r.sf.Spec.Steps {
		step := &r.sf.Spec.Steps[i]
		if step.Site == "" || !step.Deferrable || started[step.Site] || r.stepStatus(step.ID).State == failoverv1.StepSucceeded {
			continue
		}
		if _, ok := r.reachable[step.Site]; ok {
			continue
		}
		started[step.Site] = true
		once := r.sf.Spec.Mode == failoverv1.ModeSiteLoss
		wg.Add(1)
		go func(site string) {
			defer wg.Done()
			_ = r.checks.Check(ctx, site, once)
		}(step.Site)
	}
	return func() {
		cancel()
		wg.Wait()
	}
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

// lastFinish returns when the last step of a phase finished, when each of
// its steps records it; else now.
func (r *runner) lastFinish(p failoverv1.Phase, now metav1.Time) metav1.Time {
	var last metav1.Time
	for _, s := range r.sf.Spec.Steps {
		if s.Phase != p {
			continue
		}
		st := r.stepStatus(s.ID)
		if st.FinishedAt == nil {
			return now
		}
		if st.FinishedAt.After(last.Time) {
			last = *st.FinishedAt
		}
	}
	if last.IsZero() {
		return now
	}
	return last
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

// writeTimeout bounds a ledger write, its repeats included.
const writeTimeout = 30 * time.Second

// write records the status, its messages cleaned (Clean). It outlives a
// cancelled run, so an interrupt is still recorded. A write that got no answer, or an answer that the API
// server cannot serve it now, is repeated with the same resourceVersion
// until writeTimeout ([RFC#0003] Requirement 5). A repeat that conflicts
// may follow a write that landed without its answer: the ledger is read,
// and a status equal to the one sent is that write. Any other failure ends
// the process, with a message by its kind (#35 row 32).
func (e *Env) write(ctx context.Context, sf *failoverv1.SiteFailover) error {
	return e.writeEntry(ctx, sf, nil)
}

// record writes the status with entry appended to its history, then
// records entry's Event ([RFC#0003] Requirements 22 and 23).
func (e *Env) record(ctx context.Context, sf *failoverv1.SiteFailover, entry failoverv1.HistoryEntry) error {
	return e.writeEntry(ctx, sf, &entry)
}

// writeEntry writes the status as write does, with entry, when set,
// appended to its history once, however often the write is repeated. Its
// Event follows the write.
func (e *Env) writeEntry(ctx context.Context, sf *failoverv1.SiteFailover, entry *failoverv1.HistoryEntry) error {
	if entry != nil {
		AppendHistory(&sf.Status, *entry)
	}
	if err := e.update(ctx, sf); err != nil {
		return err
	}
	if rec, ok := e.Ledger.(Recorder); ok && entry != nil && len(sf.Status.History) > 0 {
		e.entryEvent(ctx, rec, sf, sf.Status.History[len(sf.Status.History)-1])
	}
	return nil
}

// joinMessage returns "id: msg", or id alone when msg is empty.
func joinMessage(id, msg string) string {
	if msg == "" {
		return id
	}
	return id + ": " + msg
}

func (e *Env) update(ctx context.Context, sf *failoverv1.SiteFailover) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	Stamp(&sf.Status)
	sent := sf.DeepCopy()
	start := e.now()
	for attempt := 1; ; attempt++ {
		err := e.Ledger.UpdateStatus(ctx, sf)
		switch {
		case err == nil:
			return nil
		case apierrors.IsConflict(err) && attempt > 1:
			if stored, gerr := e.Ledger.Get(ctx, sent.Name); gerr == nil && sameStatus(&stored.Status, &sent.Status) {
				// An earlier attempt landed; its answer was lost.
				*sf = *stored
				return nil
			}
			return fmt.Errorf("write the ledger (another tack process may be running this run): %w", err)
		case apierrors.IsConflict(err):
			return fmt.Errorf("write the ledger (another tack process may be running this run): %w", err)
		case apierrors.IsForbidden(err):
			return fmt.Errorf("write the ledger: the operator may not update sitefailovers/status in namespace %s at the ledger's site: %w", sent.Namespace, err)
		case apierrors.IsNotFound(err):
			return fmt.Errorf("write the ledger: run %s is no longer in namespace %s at the ledger's site: %w", sent.Name, sent.Namespace, err)
		case !kube.Transient(err) || ctx.Err() != nil:
			return fmt.Errorf("write the ledger: %w", err)
		}
		e.printf("  retry     the ledger write got no answer (%v); next in %s\n", err, e.poll())
		select {
		case <-ctx.Done():
			return fmt.Errorf("write the ledger: no answer to %d attempts over %s: %w", attempt, e.now().Sub(start).Round(time.Second), err)
		case <-time.After(e.poll()):
		}
		*sf = *sent.DeepCopy()
	}
}

// sameStatus compares two statuses as the API server stores them: times
// in whole seconds.
func sameStatus(a, b *failoverv1.SiteFailoverStatus) bool {
	ja, aerr := json.Marshal(a)
	jb, berr := json.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(ja, jb)
}

// printf prints a progress line, cleaned of URL credentials: it may carry
// an outside error.
func (e *Env) printf(format string, a ...any) {
	if e.Out != nil {
		fmt.Fprint(e.Out, redact.Text(fmt.Sprintf(format, a...)))
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
