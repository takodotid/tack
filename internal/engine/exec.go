package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/stop"
)

// FieldManager is the field manager of Tack's changes to live objects.
const FieldManager = "tack"

func (r *runner) exec(ctx context.Context, s *v1alpha1.Step) (string, error) {
	switch s.Type {
	case v1alpha1.StepHold:
		return r.hold(ctx, s)
	case v1alpha1.StepReconcile:
		return r.reconcile(ctx, s)
	case v1alpha1.StepPatch:
		return r.patch(ctx, s)
	case v1alpha1.StepWait:
		return r.wait(ctx, s)
	case v1alpha1.StepCapture:
		return r.capture(ctx, s)
	case v1alpha1.StepGit:
		return r.git(ctx, s)
	case v1alpha1.StepVerifyGit:
		return r.verifyGit(ctx, s)
	case v1alpha1.StepRelease:
		return r.release(ctx, s)
	case v1alpha1.StepVerifyNoop:
		return r.Flux.VerifyNoop(ctx, s.Site, r.siteHeld(s.Site), s.VerifyNoop.Fields, r.strict())
	case v1alpha1.StepRouting:
		if r.Routing == nil {
			return "", fmt.Errorf("routing is not available in this build")
		}
		return r.Routing.Wait(ctx, s.Site, s.Routing)
	}
	return "", fmt.Errorf("unknown step type %q", s.Type)
}

func (r *runner) hold(ctx context.Context, s *v1alpha1.Step) (string, error) {
	ks, err := r.Flux.Hold(ctx, s.Site, s.Hold.Object, r.sf.Name, r.opts.TakeOver)
	if err != nil {
		return "", err
	}
	now := metav1.NewTime(r.now())
	h := r.held(s.Site, s.Hold.Object)
	if h == nil {
		r.sf.Status.Held = append(r.sf.Status.Held, v1alpha1.HeldObject{Site: s.Site, Object: s.Hold.Object})
		h = &r.sf.Status.Held[len(r.sf.Status.Held)-1]
	}
	h.Kustomization, h.HeldAt, h.ReleasedAt = ks, &now, nil
	return fmt.Sprintf("held; applied by Kustomization %s/%s", ks.Namespace, ks.Name), nil
}

func (r *runner) release(ctx context.Context, s *v1alpha1.Step) (string, error) {
	gen, err := r.Flux.Release(ctx, s.Site, s.Release.Object, r.sf.Name)
	if err != nil {
		return "", err
	}
	if h := r.held(s.Site, s.Release.Object); h != nil {
		now := metav1.NewTime(r.now())
		h.ReleasedAt, h.Generation = &now, gen
	}
	return fmt.Sprintf("released at generation %d", gen), nil
}

func (r *runner) reconcile(ctx context.Context, s *v1alpha1.Step) (string, error) {
	ks := s.Reconcile.Kustomizations
	if len(ks) == 0 {
		set := map[v1alpha1.ObjectKey]bool{}
		for _, h := range r.siteHeld(s.Site) {
			if !set[h.Kustomization] {
				set[h.Kustomization] = true
				ks = append(ks, h.Kustomization)
			}
		}
	}
	rev := ""
	if s.Reconcile.Revision != "" {
		var err error
		if rev, err = r.subst(s.Reconcile.Revision); err != nil {
			return "", err
		}
	}
	applied, err := r.Flux.Reconcile(ctx, s.Site, ks, rev, s.Reconcile.Timeout.Duration)
	if err != nil {
		return "", err
	}
	if s.Phase == v1alpha1.PhaseHold {
		g := &r.sf.Status.Git
		switch {
		case applied == "":
		case g.BaseRevision == "":
			g.BaseRevision = applied
		case !SameRevision(g.BaseRevision, applied) && g.AcceptedRevision == "":
			return "", stop.Ask(v1alpha1.ReasonGitChanged,
				"Flux at site %s applied %s, but the run's base is %s: Git changed during the run", s.Site, applied, g.BaseRevision)
		}
	}
	return "Flux applied " + applied, nil
}

func (r *runner) git(ctx context.Context, s *v1alpha1.Step) (string, error) {
	g := &r.sf.Status.Git
	base := g.BaseRevision
	if g.AcceptedRevision != "" {
		base = g.AcceptedRevision
	}
	if base == "" {
		return "", stop.Ask(v1alpha1.ReasonGitChanged, "the run has no base revision")
	}
	var changes []v1alpha1.GitChange
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

func (r *runner) verifyGit(ctx context.Context, s *v1alpha1.Step) (string, error) {
	if r.sf.Status.Git.Commit == "" {
		return "", fmt.Errorf("the run has no commit yet")
	}
	dir, err := r.Git.Checkout(ctx, &r.sf.Spec.Workload.Spec, r.sf.Status.Git.Commit)
	if err != nil {
		return "", err
	}
	return r.Flux.VerifyGit(ctx, s.Site, r.siteHeld(s.Site), s.VerifyGit.Fields, dir, r.strict())
}

// strict is false once the operator accepted a Git commit made during the
// run: that commit may change the held objects in other fields.
func (r *runner) strict() bool { return r.sf.Status.Git.AcceptedRevision == "" }

func (r *runner) held(site string, obj v1alpha1.ObjectRef) *v1alpha1.HeldObject {
	for i := range r.sf.Status.Held {
		h := &r.sf.Status.Held[i]
		if h.Site == site && h.Object == obj {
			return h
		}
	}
	return nil
}

func (r *runner) siteHeld(site string) []v1alpha1.HeldObject {
	var out []v1alpha1.HeldObject
	for _, h := range r.sf.Status.Held {
		if h.Site == site {
			out = append(out, h)
		}
	}
	return out
}

func (r *runner) patch(ctx context.Context, s *v1alpha1.Step) (string, error) {
	body, err := r.subst(s.Patch.Merge)
	if err != nil {
		return "", err
	}
	if !json.Valid([]byte(body)) {
		return "", fmt.Errorf("patch is not valid JSON")
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

func (r *runner) wait(ctx context.Context, s *v1alpha1.Step) (string, error) {
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

func (r *runner) conditionsHold(u *unstructured.Unstructured, conds []v1alpha1.Condition) (bool, string, error) {
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
		case v1alpha1.OpEquals:
			ok = got == want
		case v1alpha1.OpNotEquals:
			ok = got != want
		case v1alpha1.OpNotEmpty:
			ok = got != ""
		case v1alpha1.OpEmpty:
			ok = got == ""
		default:
			return false, "", fmt.Errorf("unknown operator %q", cond.Operator)
		}
		if !ok {
			return false, fmt.Sprintf("%s is %q, want %s %q", cond.JSONPath, got, cond.Operator, want), nil
		}
	}
	return true, "", nil
}

func (r *runner) capture(ctx context.Context, s *v1alpha1.Step) (string, error) {
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
		return "", stop.Ask(v1alpha1.ReasonTokenMissing, "%s has no value at %s", refString(s.Capture.Object), s.Capture.JSONPath)
	}
	if r.sf.Status.Captured == nil {
		r.sf.Status.Captured = map[string]string{}
	}
	r.sf.Status.Captured[s.Capture.Into] = v
	return fmt.Sprintf("captured %s (%d bytes)", s.Capture.Into, len(v)), nil
}

var placeholder = regexp.MustCompile(`\$\(([a-zA-Z0-9_.-]+)\)`)

// subst replaces $(name) with the captured value of that name, and
// $(git.commit) with the run's commit.
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
		v, ok := r.sf.Status.Captured[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", stop.Ask(v1alpha1.ReasonTokenMissing, "the ledger has no value for %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func newObject(ref v1alpha1.ObjectRef) *unstructured.Unstructured {
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
