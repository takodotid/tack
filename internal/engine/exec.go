package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/stop"
)

// FieldManager is the field manager of Tack's changes to live objects.
const FieldManager = "tack"

func (r *runner) exec(ctx context.Context, s *failoverv1.Step) (string, error) {
	switch s.Type {
	case failoverv1.StepHold:
		return r.hold(ctx, s)
	case failoverv1.StepReconcile:
		return r.reconcile(ctx, s)
	case failoverv1.StepPatch:
		return r.patch(ctx, s)
	case failoverv1.StepWait:
		return r.wait(ctx, s)
	case failoverv1.StepCapture:
		return r.capture(ctx, s)
	case failoverv1.StepGit:
		return r.git(ctx, s)
	case failoverv1.StepVerifyGit:
		return r.verifyGit(ctx, s)
	case failoverv1.StepRelease:
		return r.release(ctx, s)
	case failoverv1.StepVerifyNoop:
		return r.Flux.VerifyNoop(ctx, s.Site, r.siteHeld(s.Site), s.VerifyNoop.Fields, r.strict(), r.fluxTimeout())
	case failoverv1.StepRouting:
		if r.Routing == nil {
			return "", fmt.Errorf("routing is not available in this build")
		}
		return r.Routing.Wait(ctx, s.Site, s.Routing)
	}
	return "", fmt.Errorf("unknown step type %q", s.Type)
}

func (r *runner) hold(ctx context.Context, s *failoverv1.Step) (string, error) {
	takeOver := r.sf.Spec.TakeOver
	if takeOver == "" {
		takeOver = r.opts.TakeOver
	}
	ks, err := r.Flux.Hold(ctx, s.Site, s.Hold.Object, r.sf.Name, takeOver)
	if err != nil {
		return "", err
	}
	now := metav1.NewTime(r.now())
	h := r.held(s.Site, s.Hold.Object)
	if h == nil {
		r.sf.Status.Held = append(r.sf.Status.Held, failoverv1.HeldObject{Site: s.Site, Object: s.Hold.Object})
		h = &r.sf.Status.Held[len(r.sf.Status.Held)-1]
	}
	h.Kustomization, h.HeldAt, h.ReleasedAt, h.Generation = ks, &now, nil, 0
	return fmt.Sprintf("held; applied by Kustomization %s/%s", ks.Namespace, ks.Name), nil
}

// release removes the run's hold on an object. The ledger records the
// object's generation before the hold is removed, while Flux cannot change
// the object ([RFC#0005] Requirement 9). A process that dies after the
// removal, before it records the release, leaves that generation: the
// next process finds no hold, and keeps it, so the check after release
// sees what Flux changed in between (#35 row 36).
func (r *runner) release(ctx context.Context, s *failoverv1.Step) (string, error) {
	recorded := false
	gen, err := r.Flux.Release(ctx, s.Site, s.Release.Object, r.sf.Name, func(gen int64) error {
		h := r.held(s.Site, s.Release.Object)
		if h == nil {
			return nil
		}
		h.Generation, recorded = gen, true
		return r.write(ctx, r.sf)
	})
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("released at generation %d", gen)
	h := r.held(s.Site, s.Release.Object)
	if h == nil {
		return msg, nil
	}
	if !recorded {
		msg += " (the hold was already gone)"
		if h.Generation != 0 && h.Generation != gen {
			msg = fmt.Sprintf("the hold was already gone: generation %d when the run removed it, %d now", h.Generation, gen)
			gen = h.Generation
		}
	}
	now := metav1.NewTime(r.now())
	h.ReleasedAt, h.Generation = &now, gen
	return msg, nil
}

func (r *runner) reconcile(ctx context.Context, s *failoverv1.Step) (string, error) {
	ks := s.Reconcile.Kustomizations
	if len(ks) == 0 {
		set := map[failoverv1.ObjectKey]bool{}
		for _, h := range r.siteHeld(s.Site) {
			if !set[h.Kustomization] {
				set[h.Kustomization] = true
				ks = append(ks, h.Kustomization)
			}
		}
	}
	rev, moved := "", ""
	switch s.Reconcile.Revision {
	case "":
	case GitCommitRef:
		var err error
		if rev, moved, err = r.awaited(ctx); err != nil {
			return "", err
		}
	default:
		var err error
		if rev, err = r.subst(s.Reconcile.Revision); err != nil {
			return "", err
		}
	}
	fetch := FetchesGit(r.sf.Spec.Mode, s.Phase)
	applied, notes, err := r.Flux.Reconcile(ctx, s.Site, ks, rev, fetch, s.Reconcile.Timeout.Duration)
	if err != nil {
		return "", err
	}
	msg := "Flux applied " + applied
	if applied == "" {
		msg = "nothing to reconcile"
	}
	if moved != "" {
		msg += "; " + moved
	}
	if !fetch {
		notes = append([]string{"Git not fetched: a site-loss run holds without Git"}, notes...)
	}
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	if s.Phase == failoverv1.PhaseHold {
		if err := r.checkBase(s.Site, applied); err != nil {
			return "", err
		}
	}
	return msg, nil
}

// checkBase checks the revision Flux applied at a site in Hold against the
// run's base. Once the run committed, a site that comes back may have
// applied the run's commit, or the head the operator accepted after it:
// Flux there follows Git, which already declares the switch. Both pass
// without reading Git, which Hold must not need ([RFC#0007] Requirement 1);
// the Release phase checks the branch before it releases the site.
func (r *runner) checkBase(site, applied string) error {
	g := &r.sf.Status.Git
	switch {
	case applied == "":
		return nil
	case g.BaseRevision == "":
		g.BaseRevision = applied
		return nil
	case SameRevision(g.BaseRevision, applied) || g.AcceptedRevision != "":
		return nil
	case g.Commit == "":
		return stop.Ask(failoverv1.ReasonGitChanged,
			"Flux at site %s applied %s, but the run's base is %s: Git changed during the run", site, applied, g.BaseRevision)
	}
	sha := RevisionSHA(applied)
	if sameCommit(sha, g.Commit) || g.ReleaseRevision != "" && sameCommit(sha, g.ReleaseRevision) {
		return nil
	}
	return stop.Ask(failoverv1.ReasonGitChanged,
		"Flux at site %s applied %s, which is neither the run's base %s nor its commit %s: Git changed during the run. Review it; if it contains the run's commit, resume with --accept-git-head %s",
		site, applied, g.BaseRevision, g.Commit, sha)
}

func (r *runner) git(ctx context.Context, s *failoverv1.Step) (string, error) {
	g := &r.sf.Status.Git
	base := g.BaseRevision
	if g.AcceptedRevision != "" {
		base = g.AcceptedRevision
	}
	if base == "" {
		return "", stop.Ask(failoverv1.ReasonGitChanged, "the run has no base revision")
	}
	var changes []failoverv1.GitChange
	for _, c := range s.Git.Changes {
		c := *c.DeepCopy()
		if c.Value != nil {
			v, err := r.subst(*c.Value)
			if err != nil {
				return "", err
			}
			c.Value = &v
		}
		changes = append(changes, c)
	}
	commit, err := r.Git.Commit(ctx, &r.sf.Spec.Workload.Spec, CommitRequest{
		Run: r.sf.Name, Base: RevisionSHA(base), Changes: changes, Message: s.Git.Message,
	})
	if err != nil {
		return "", err
	}
	g.Commit = commit
	return "committed " + commit, nil
}

// releaseTarget is the commit the Release phase checks Git against: the
// head the operator accepted after the run's commit, or that commit.
func (r *runner) releaseTarget() string {
	if g := r.sf.Status.Git; g.ReleaseRevision != "" {
		return g.ReleaseRevision
	}
	return r.sf.Status.Git.Commit
}

// awaited returns the commit that a Reconcile after the run's commit waits
// for: the branch head, which is what Flux fetches, when it contains the
// release target. A push on top of the run's commit then does not leave the
// wait stuck, and the check before release still sees it. moved says so
// when the head is past the target.
func (r *runner) awaited(ctx context.Context) (rev, moved string, err error) {
	want := r.releaseTarget()
	if want == "" {
		return "", "", fmt.Errorf("the run has no commit yet")
	}
	w := &r.sf.Spec.Workload.Spec
	head, err := r.Git.BranchHead(ctx, w)
	if err != nil {
		return "", "", err
	}
	if sameCommit(head, want) {
		return head, "", nil
	}
	ok, err := r.Git.Contains(ctx, w, want, head)
	switch {
	case err != nil:
		return "", "", err
	case !ok:
		return "", "", stop.Ask(failoverv1.ReasonGitChanged,
			"branch %s of %s is at %s, which does not contain %s: Git was rewritten during the run. Review it, then resume with --accept-git-head on a commit that contains the switch",
			w.Git.Branch, Redact(w.Git.URL), head, want)
	}
	return head, fmt.Sprintf("the branch moved past %s to %s", want, head), nil
}

// verifyGit checks Git against live at the release target, and stops when
// the branch moved past it: once released, the objects get whatever Flux
// applies.
func (r *runner) verifyGit(ctx context.Context, s *failoverv1.Step) (string, error) {
	g := r.sf.Status.Git
	if g.Commit == "" {
		return "", fmt.Errorf("the run has no commit yet")
	}
	w := &r.sf.Spec.Workload.Spec
	want := r.releaseTarget()
	if g.ReleaseRevision != "" {
		ok, err := r.Git.Contains(ctx, w, g.Commit, g.ReleaseRevision)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", stop.Ask(failoverv1.ReasonGitChanged,
				"the accepted head %s does not contain the run's commit %s: accept a head that does", g.ReleaseRevision, g.Commit)
		}
	}
	head, err := r.Git.BranchHead(ctx, w)
	if err != nil {
		return "", err
	}
	if !sameCommit(head, want) {
		ok, err := r.Git.Contains(ctx, w, want, head)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", stop.Ask(failoverv1.ReasonGitChanged,
				"branch %s of %s is at %s, which does not contain %s: Git was rewritten before the release at site %s. Review it, then resume with --accept-git-head on a commit that contains the switch",
				w.Git.Branch, Redact(w.Git.URL), head, want, s.Site)
		}
		return "", stop.Ask(failoverv1.ReasonGitChanged,
			"branch %s of %s moved past %s to %s before the release at site %s: Flux applies it once the objects are released. Review the commits (git log %s..%s), then resume with --accept-git-head %s",
			w.Git.Branch, Redact(w.Git.URL), want, head, s.Site, want, head, head)
	}
	dir, err := r.Git.Checkout(ctx, w, want)
	if err != nil {
		return "", err
	}
	return r.Flux.VerifyGit(ctx, s.Site, r.siteHeld(s.Site), s.VerifyGit.Fields, dir, r.strict())
}

// fluxTimeout bounds a wait on Flux outside a Reconcile step, which
// carries its own: the Workload's timeouts.flux, or its default.
func (r *runner) fluxTimeout() time.Duration {
	if t := r.sf.Spec.Workload.Spec.Timeouts.Flux; t != nil && t.Duration > 0 {
		return t.Duration
	}
	return config.DefaultFlux
}

// strict is false once the operator accepted a Git commit made during the
// run: that commit may change the held objects in other fields.
func (r *runner) strict() bool {
	g := r.sf.Status.Git
	return g.AcceptedRevision == "" && g.ReleaseRevision == ""
}

// sameCommit compares two commit ids, either of which may be abbreviated.
func sameCommit(a, b string) bool {
	a, b = RevisionSHA(a), RevisionSHA(b)
	if len(a) < 4 || len(b) < 4 {
		return a == b
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func (r *runner) held(site string, obj failoverv1.ObjectRef) *failoverv1.HeldObject {
	for i := range r.sf.Status.Held {
		h := &r.sf.Status.Held[i]
		if h.Site == site && h.Object == obj {
			return h
		}
	}
	return nil
}

func (r *runner) siteHeld(site string) []failoverv1.HeldObject {
	var out []failoverv1.HeldObject
	for _, h := range r.sf.Status.Held {
		if h.Site == site {
			out = append(out, h)
		}
	}
	return out
}

func (r *runner) patch(ctx context.Context, s *failoverv1.Step) (string, error) {
	body, err := r.subst(s.Patch.Merge)
	if err != nil {
		return "", err
	}
	if !json.Valid([]byte(body)) {
		return "", fmt.Errorf("patch is not valid JSON")
	}
	if err := r.checkHeld(ctx, s.Site, s.Patch.Object); err != nil {
		return "", err
	}
	c, err := r.Clients.Client(s.Site)
	if err != nil {
		return "", err
	}
	u := newObject(s.Patch.Object)
	err = c.Patch(ctx, u, client.RawPatch(types.MergePatchType, []byte(body)), client.FieldOwner(FieldManager))
	if err != nil {
		return "", fmt.Errorf("patch %s: %w", refString(s.Patch.Object), err)
	}
	return fmt.Sprintf("patched; generation %d", u.GetGeneration()), nil
}

// checkHeld stops the run before it patches an object it holds, when the
// object's hold no longer names the run: another run took it over, or the
// hold is gone and Flux could apply Git over the patch. A hold removed by
// hand may come with a recovery by hand, after which the plan no longer
// fits the sites (issue #45).
func (r *runner) checkHeld(ctx context.Context, site string, obj failoverv1.ObjectRef) error {
	planned := false
	for _, s := range r.sf.Spec.Steps {
		planned = planned || s.Type == failoverv1.StepHold && s.Site == site && s.Hold.Object == obj
	}
	if !planned {
		return nil
	}
	by, err := r.Flux.HeldBy(ctx, site, obj)
	switch {
	case err != nil:
		return err
	case by == r.sf.Name:
		return nil
	case by == "":
		return stop.Ask(failoverv1.ReasonStepFailed,
			"%s at site %s is no longer held by this run, so Flux may apply Git over the change. Find who removed the hold, and why. If the workload was changed by hand, such as by a recovery after the run stopped, the run's plan may be stale: do not resume it, see docs/runbooks/abandon-run.md. Only if nothing else changed, annotate it again with kustomize.toolkit.fluxcd.io/reconcile=disabled and failover.tako.id/held-by=%s, then resume",
			refString(obj), site, r.sf.Name)
	}
	return stop.Ask(failoverv1.ReasonHeldByAnotherRun,
		"%s at site %s is held by run %s, which took it over: this run must not change it. See tack status %s", refString(obj), site, by, by)
}

func (r *runner) wait(ctx context.Context, s *failoverv1.Step) (string, error) {
	c, err := r.Clients.Client(s.Site)
	if err != nil {
		return "", err
	}
	deadline := r.now().Add(s.Wait.Timeout.Duration)
	var last string
	for {
		u := newObject(s.Wait.Object)
		err := c.Get(ctx, client.ObjectKeyFromObject(u), u)
		if err == nil {
			ok, why, cerr := r.conditionsHold(u, s.Wait.Conditions)
			if cerr != nil {
				return "", cerr
			}
			if ok {
				return "conditions hold", nil
			}
			last = why
		} else {
			last = err.Error()
		}
		if r.now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for %s: %s", s.Wait.Timeout.Duration, refString(s.Wait.Object), last)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(r.poll()):
		}
	}
}

func (r *runner) conditionsHold(u *unstructured.Unstructured, conds []failoverv1.Condition) (bool, string, error) {
	for _, cond := range conds {
		got, err := JSONPath(u.Object, cond.JSONPath)
		if err != nil {
			return false, "", err
		}
		want, err := r.subst(cond.Value)
		if err != nil {
			return false, "", err
		}
		ok := false
		switch cond.Operator {
		case failoverv1.OpEquals:
			ok = got == want
		case failoverv1.OpNotEquals:
			ok = got != want
		case failoverv1.OpNotEmpty:
			ok = got != ""
		case failoverv1.OpEmpty:
			ok = got == ""
		case failoverv1.OpContains:
			ok = slices.Contains(strings.Fields(got), want)
		default:
			return false, "", fmt.Errorf("unknown operator %q", cond.Operator)
		}
		if !ok {
			if placeholder.MatchString(cond.Value) {
				// The value is captured, such as a token: name it, and
				// print neither it nor the live value it is compared with.
				return false, fmt.Sprintf("%s is %s, want %s %s", cond.JSONPath, size(got), cond.Operator, cond.Value), nil
			}
			return false, fmt.Sprintf("%s is %q, want %s %q", cond.JSONPath, got, cond.Operator, want), nil
		}
	}
	return true, "", nil
}

func (r *runner) capture(ctx context.Context, s *failoverv1.Step) (string, error) {
	c, err := r.Clients.Client(s.Site)
	if err != nil {
		return "", err
	}
	u := newObject(s.Capture.Object)
	if err := c.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
		return "", err
	}
	v, err := JSONPath(u.Object, s.Capture.JSONPath)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", stop.Ask(failoverv1.ReasonTokenMissing, "%s has no value at %s", refString(s.Capture.Object), s.Capture.JSONPath)
	}
	if r.sf.Status.Captured == nil {
		r.sf.Status.Captured = map[string]string{}
	}
	r.sf.Status.Captured[s.Capture.Into] = v
	return fmt.Sprintf("captured %s (%d bytes)", s.Capture.Into, len(v)), nil
}

// size describes a value without printing it.
func size(v string) string {
	if v == "" {
		return "empty"
	}
	return fmt.Sprintf("a value of %d bytes", len(v))
}

// minHidden is the shortest captured value hideCaptured replaces: a shorter
// one is no token, and replacing it would garble the message.
const minHidden = 8

// hideCaptured replaces each captured value in a message with its name, so
// the ledger and the output name captured values and never print them
// ([RFC#0009] Requirement 10), whatever error carried one.
func (r *runner) hideCaptured(msg string) string { return HideCaptured(r.sf.Status.Captured, msg) }

// HideCaptured replaces each value of captured, of minHidden bytes or more,
// in msg with $(name). tack status uses it too, for messages that an
// older Tack wrote with a value in them.
func HideCaptured(captured map[string]string, msg string) string {
	names := make([]string, 0, len(captured))
	for name := range captured {
		names = append(names, name)
	}
	// The longest value first, so a value inside another is not replaced
	// first.
	slices.SortFunc(names, func(a, b string) int { return len(captured[b]) - len(captured[a]) })
	for _, name := range names {
		if v := captured[name]; len(v) >= minHidden {
			msg = strings.ReplaceAll(msg, v, "$("+name+")")
		}
	}
	return msg
}

var placeholder = regexp.MustCompile(`\$\(([a-zA-Z0-9_.-]+)\)`)

// subst replaces $(name) with the captured value of that name, and
// $(git.commit) with the run's commit. An absent or empty value stops the
// run with TokenMissing: a promotion without its token would not wait for
// the demotion's checkpoint.
func (r *runner) subst(s string) (string, error) {
	var missing []string
	out := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		name := placeholder.FindStringSubmatch(m)[1]
		if name == "git.commit" {
			if r.sf.Status.Git.Commit == "" {
				missing = append(missing, name)
			}
			return r.sf.Status.Git.Commit
		}
		v := r.sf.Status.Captured[name]
		if v == "" {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", stop.Ask(failoverv1.ReasonTokenMissing, "the ledger has no value for %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func newObject(ref failoverv1.ObjectRef) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
	u.SetNamespace(ref.Namespace)
	u.SetName(ref.Name)
	return u
}

// JSONPath evaluates a JSONPath expression such as {.status.phase} on an
// object. A missing field gives an empty string.
func JSONPath(obj map[string]any, expr string) (string, error) {
	jp := jsonpath.New("tack").AllowMissingKeys(true)
	if err := jp.Parse(expr); err != nil {
		return "", fmt.Errorf("jsonpath %q: %w", expr, err)
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, obj); err != nil {
		return "", fmt.Errorf("jsonpath %q: %w", expr, err)
	}
	return buf.String(), nil
}

// RevisionSHA returns the commit of a Flux revision such as main@sha1:abc.
func RevisionSHA(rev string) string {
	if i := strings.LastIndex(rev, ":"); i >= 0 {
		return rev[i+1:]
	}
	if i := strings.LastIndex(rev, "@"); i >= 0 {
		return rev[i+1:]
	}
	return rev
}

// SameRevision compares two revisions by commit.
func SameRevision(a, b string) bool { return RevisionSHA(a) == RevisionSHA(b) }
