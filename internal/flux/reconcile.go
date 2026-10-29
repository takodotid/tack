package flux

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/stop"
)

// ReasonSucceeded is the Ready reason of a Kustomization run that applied
// its revision.
const ReasonSucceeded = "ReconciliationSucceeded"

// Reconcile implements engine.Flux. It requests each source GitRepository,
// waits until each handled the request or half the timeout passed, then
// requests each Kustomization and waits until each is done. A source that
// fails to fetch does not stop the wait: the Kustomization runs on the
// artifact it has, which is enough when no revision is expected.
func (f *Flux) Reconcile(ctx context.Context, site string, ks []failoverv1.ObjectKey, revision string, timeout time.Duration) (string, []string, error) {
	var notes []string
	note := func(format string, a ...any) {
		n := fmt.Sprintf(format, a...)
		notes = append(notes, n)
		f.note("%s", n)
	}
	c, err := f.Clients.Client(site)
	if err != nil {
		return "", notes, err
	}
	deadline := f.now().Add(timeout)

	var active []failoverv1.ObjectKey
	var sources []failoverv1.ObjectKey
	seen, seenSource := map[failoverv1.ObjectKey]bool{}, map[failoverv1.ObjectKey]bool{}
	for _, k := range ks {
		if seen[k] {
			continue
		}
		seen[k] = true
		u, err := getKind(ctx, c, KustomizationGVK, k)
		if err != nil {
			return "", notes, err
		}
		if suspended(u) {
			note("Kustomization %s at site %s is suspended: not reconciled", keyString(k), site)
			continue
		}
		active = append(active, k)
		if kind, src := sourceRef(u); kind == GitRepositoryGVK.Kind && !seenSource[src] {
			seenSource[src] = true
			sources = append(sources, src)
		}
	}
	if len(active) == 0 {
		return "", notes, nil
	}

	// Fetch Git first, so the Kustomizations run on the newest artifact.
	pending := map[failoverv1.ObjectKey]string{}
	for _, s := range sources {
		u, err := getKind(ctx, c, GitRepositoryGVK, s)
		if err != nil {
			return "", notes, err
		}
		if suspended(u) {
			note("GitRepository %s at site %s is suspended: not fetched", keyString(s), site)
			continue
		}
		tok, err := f.request(ctx, c, u)
		if err != nil {
			return "", notes, err
		}
		pending[s] = tok
	}
	sourceDeadline := f.now().Add(timeout / 2)
	for len(pending) > 0 {
		for s, tok := range pending {
			// A failed read is retried until the deadline, as a wait does.
			u, err := getKind(ctx, c, GitRepositoryGVK, s)
			if err != nil || handled(u) != tok {
				continue
			}
			delete(pending, s)
			if st, reason, msg := ready(u); st != "True" {
				note("GitRepository %s at site %s did not fetch (%s: %s); Flux applies the artifact it has, %s",
					keyString(s), site, reason, msg, artifactRevision(u))
			}
		}
		if len(pending) == 0 {
			break
		}
		if f.now().After(sourceDeadline) {
			for s := range pending {
				note("GitRepository %s at site %s did not handle the reconcile request in %s", keyString(s), site, timeout/2)
			}
			break
		}
		if err := f.sleep(ctx); err != nil {
			return "", notes, err
		}
	}

	tokens := map[failoverv1.ObjectKey]string{}
	for _, k := range active {
		u, err := getKind(ctx, c, KustomizationGVK, k)
		if err != nil {
			return "", notes, err
		}
		tok, err := f.request(ctx, c, u)
		if err != nil {
			return "", notes, err
		}
		tokens[k] = tok
	}
	applied := map[failoverv1.ObjectKey]string{}
	why := map[failoverv1.ObjectKey]string{}
	for {
		for _, k := range active {
			tok, waiting := tokens[k]
			if !waiting {
				continue
			}
			u, err := getKind(ctx, c, KustomizationGVK, k)
			if err != nil {
				why[k] = err.Error()
				continue
			}
			if suspended(u) {
				note("Kustomization %s at site %s was suspended during the wait: not reconciled", keyString(k), site)
				delete(tokens, k)
				continue
			}
			ok, w := done(u, tok, revision)
			if ok {
				applied[k] = str(u, "status", "lastAppliedRevision")
				delete(tokens, k)
				continue
			}
			why[k] = w
		}
		if len(tokens) == 0 {
			break
		}
		if f.now().After(deadline) {
			var parts []string
			for _, k := range active {
				if _, waiting := tokens[k]; waiting {
					parts = append(parts, fmt.Sprintf("Kustomization %s: %s", keyString(k), why[k]))
				}
			}
			return "", notes, fmt.Errorf("timed out after %s waiting for Flux at site %s: %s", timeout, site, strings.Join(parts, "; "))
		}
		if err := f.sleep(ctx); err != nil {
			return "", notes, err
		}
	}

	rev, first := "", failoverv1.ObjectKey{}
	for _, k := range active {
		r, ok := applied[k]
		if !ok {
			continue
		}
		if rev == "" {
			rev, first = r, k
			continue
		}
		if !engine.SameRevision(rev, r) {
			return "", notes, stop.Ask(failoverv1.ReasonGitChanged,
				"Kustomizations at site %s applied different revisions: %s applied %s, %s applied %s",
				site, keyString(first), rev, keyString(k), r)
		}
	}
	return rev, notes, nil
}

// request sets a fresh reconcile token on a Flux object and returns it.
func (f *Flux) request(ctx context.Context, c client.Client, u *unstructured.Unstructured) (string, error) {
	t := f.now()
	tok := t.Format(time.RFC3339Nano)
	for tok == u.GetAnnotations()[AnnotationRequestedAt] || tok == handled(u) {
		t = t.Add(time.Nanosecond)
		tok = t.Format(time.RFC3339Nano)
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{AnnotationRequestedAt: tok}},
	})
	if err != nil {
		return "", err
	}
	if err := c.Patch(ctx, u, client.RawPatch(types.MergePatchType, body), client.FieldOwner(engine.FieldManager)); err != nil {
		return "", fmt.Errorf("request a reconcile of %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
	}
	return tok, nil
}

// done is true when the Kustomization finished a successful run after the
// request tok, at revision when it is set. Otherwise it says why not.
func done(u *unstructured.Unstructured, tok, revision string) (bool, string) {
	st, reason, msg := ready(u)
	state := fmt.Sprintf("Ready=%s %s: %s", st, reason, msg)
	if h := handled(u); h != tok {
		return false, "has not handled the reconcile request yet; " + state
	}
	if g, og := u.GetGeneration(), integer(u, "status", "observedGeneration"); g != og {
		return false, fmt.Sprintf("observed generation %d, want %d; %s", og, g, state)
	}
	if st != "True" || reason != ReasonSucceeded {
		return false, state
	}
	applied := str(u, "status", "lastAppliedRevision")
	if revision != "" && !engine.SameRevision(applied, revision) {
		return false, fmt.Sprintf("applied %s, want %s; %s", applied, revision, state)
	}
	return true, ""
}

// ready returns the status, reason and message of the Ready condition.
func ready(u *unstructured.Unstructured) (string, string, string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		s, _ := m["status"].(string)
		r, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		return s, r, msg
	}
	return "Unknown", "NoReadyCondition", "no Ready condition"
}

func suspended(u *unstructured.Unstructured) bool {
	b, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend")
	return b
}

func handled(u *unstructured.Unstructured) string {
	return str(u, "status", "lastHandledReconcileAt")
}

func artifactRevision(u *unstructured.Unstructured) string {
	if r := str(u, "status", "artifact", "revision"); r != "" {
		return r
	}
	return "none"
}

// sourceRef returns the kind and key of a Kustomization's source. The
// source's namespace defaults to the Kustomization's.
func sourceRef(u *unstructured.Unstructured) (string, failoverv1.ObjectKey) {
	k := failoverv1.ObjectKey{
		Namespace: str(u, "spec", "sourceRef", "namespace"),
		Name:      str(u, "spec", "sourceRef", "name"),
	}
	if k.Namespace == "" {
		k.Namespace = u.GetNamespace()
	}
	return str(u, "spec", "sourceRef", "kind"), k
}

func str(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

func integer(u *unstructured.Unstructured, fields ...string) int64 {
	v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, fields...)
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
