package flux

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fluxcd/pkg/ssa"
	"github.com/fluxcd/pkg/ssa/jsondiff"
	"github.com/fluxcd/pkg/ssa/normalize"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/stop"
)

// differ runs kustomize-controller's dry-run apply of one object. The real
// one needs an API server; tests replace it.
type differ interface {
	Diff(ctx context.Context, c client.Client, obj *unstructured.Unstructured, opts ssa.DiffOptions) (diffResult, error)
}

// diffResult is what applying an object would do.
type diffResult struct {
	// Action is unchanged, configured, created or skipped.
	Action ssa.Action
	// Live and Merged are the object before and after the apply, without
	// managed fields. They are set only when the apply changes the object.
	Live, Merged *unstructured.Unstructured
}

// ssaDiffer diffs with fluxcd/pkg/ssa, as kustomize-controller does.
type ssaDiffer struct{}

func (ssaDiffer) Diff(ctx context.Context, c client.Client, obj *unstructured.Unstructured, opts ssa.DiffOptions) (diffResult, error) {
	e, live, merged, err := ssa.NewResourceManager(c, nil, controllerOwner).Diff(ctx, obj, opts)
	if err != nil {
		return diffResult{}, err
	}
	return diffResult{Action: e.Action, Live: live, Merged: merged}, nil
}

// diffOptions mirrors kustomize-controller's apply options for drift,
// without the exclusion of held objects.
func diffOptions(ks *unstructured.Unstructured) (ssa.DiffOptions, error) {
	force, _, _ := unstructured.NestedBool(ks.Object, "spec", "force")
	opts := ssa.DiffOptions{
		Force:         force,
		ForceSelector: map[string]string{KustomizationGVK.Group + "/force": "enabled"},
	}
	raw, ok, _ := unstructured.NestedSlice(ks.Object, "spec", "ignore")
	if !ok {
		return opts, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return opts, err
	}
	var rules []struct {
		Paths  []string `json:"paths"`
		Target *struct {
			Group              string `json:"group"`
			Version            string `json:"version"`
			Kind               string `json:"kind"`
			Name               string `json:"name"`
			Namespace          string `json:"namespace"`
			AnnotationSelector string `json:"annotationSelector"`
			LabelSelector      string `json:"labelSelector"`
		} `json:"target"`
	}
	if err := json.Unmarshal(b, &rules); err != nil {
		return opts, fmt.Errorf("Kustomization %s/%s: spec.ignore: %w", ks.GetNamespace(), ks.GetName(), err)
	}
	for _, r := range rules {
		rule := jsondiff.IgnoreRule{Paths: r.Paths}
		if t := r.Target; t != nil {
			rule.Selector = &jsondiff.Selector{
				Group: t.Group, Version: t.Version, Kind: t.Kind, Name: t.Name, Namespace: t.Namespace,
				AnnotationSelector: t.AnnotationSelector, LabelSelector: t.LabelSelector,
			}
		}
		opts.DriftIgnoreRules = append(opts.DriftIgnoreRules, rule)
	}
	return opts, nil
}

// VerifyGit implements engine.Flux. For each Kustomization that applies a
// held object, it renders the Kustomization from dir and dry-runs the held
// objects as kustomize-controller.
func (f *Flux) VerifyGit(ctx context.Context, site string, held []v1alpha1.HeldObject, fields []v1alpha1.ObjectField, dir string, strict bool) (string, error) {
	if len(held) == 0 {
		return "no object is held at site " + site, nil
	}
	c, err := f.Clients.Client(site)
	if err != nil {
		return "", err
	}
	var order []v1alpha1.ObjectKey
	byKs := map[v1alpha1.ObjectKey][]v1alpha1.ObjectRef{}
	for _, h := range held {
		k := h.Kustomization
		if k.Name == "" {
			return "", fmt.Errorf("the ledger has no Kustomization for %s at site %s", refString(h.Object), site)
		}
		if _, ok := byKs[k]; !ok {
			order = append(order, k)
		}
		byKs[k] = append(byKs[k], h.Object)
	}
	mask := &masker{}
	var objs []checked
	var missing []string
	for _, k := range order {
		ks, err := getKind(ctx, c, KustomizationGVK, k)
		if err != nil {
			return "", err
		}
		r, err := render(ctx, c, ks, dir, byKs[k])
		if err != nil {
			return "", err
		}
		mask.merge(r.Mask)
		opts, err := diffOptions(ks)
		if err != nil {
			return "", err
		}
		for _, ref := range byKs[k] {
			git, ok := r.Objects[refID(ref)]
			if !ok {
				missing = append(missing, fmt.Sprintf("%s (Kustomization %s, path %q)", refString(ref), keyString(k), str(ks, "spec", "path")))
				continue
			}
			live, err := getObject(ctx, c, ref)
			if err != nil {
				return "", err
			}
			res, err := f.diff().Diff(ctx, c, git.DeepCopy(), opts)
			if err != nil {
				return "", mask.err(fmt.Errorf("dry-run apply of %s at site %s as %s: %w", refString(ref), site, ControllerFieldManager, err))
			}
			objs = append(objs, checked{Ref: ref, Git: git, Live: live, Result: res})
		}
	}
	if len(missing) > 0 {
		return "", stop.Ask(v1alpha1.ReasonGitDiffersFromLive,
			"Git, as Flux renders it at site %s, lacks %s", site, strings.Join(missing, "; "))
	}
	fs, err := classify(objs, fields, mask)
	if err != nil {
		return "", err
	}
	return verdict(fs, len(objs), site, strict)
}

// checked is one held object: rendered from Git, live, and the dry-run.
type checked struct {
	Ref    v1alpha1.ObjectRef
	Git    *unstructured.Unstructured
	Live   *unstructured.Unstructured
	Result diffResult
}

// findings are the differences between Git and live.
type findings struct {
	// Fields has one line per differing field this run changed, with
	// both values.
	Fields []string
	// Other has one line per object with the other differing paths,
	// without values: they may hold substituted Secrets.
	Other []string
}

// classify sorts the differences into the run's fields and the rest.
func classify(objs []checked, fields []v1alpha1.ObjectField, mask *masker) (findings, error) {
	var fs findings
	byObj := map[objID][]path{}
	names := map[objID][]string{}
	for _, fl := range fields {
		p, err := parsePath(fl.Field)
		if err != nil {
			return fs, fmt.Errorf("%s: %w", refString(fl.Object), err)
		}
		id := refID(fl.Object)
		byObj[id] = append(byObj[id], p)
		names[id] = append(names[id], fl.Field)
	}
	seen := map[objID]bool{}
	for _, o := range objs {
		id := refID(o.Ref)
		seen[id] = true
		var drift []path
		unknown := false
		switch o.Result.Action {
		case ssa.UnchangedAction:
		case ssa.ConfiguredAction:
			if o.Result.Live == nil || o.Result.Merged == nil {
				unknown = true
				break
			}
			drift = driftPaths(o.Result.Live, o.Result.Merged)
			unknown = len(drift) == 0
		case ssa.CreatedAction:
			return fs, stop.Ask(v1alpha1.ReasonGitDiffersFromLive,
				"applying Git would create %s again: it is missing live, or a field that cannot change differs", refString(o.Ref))
		default:
			return fs, fmt.Errorf("dry-run apply of %s: unexpected result %q", refString(o.Ref), o.Result.Action)
		}
		value := func(v any, ok bool) string {
			if ok && id.Group == "" && id.Kind == "Secret" {
				return "(a Secret value)"
			}
			return mask.mask(show(v, ok))
		}
		for i, fp := range byObj[id] {
			gv, gok := lookup(o.Git.Object, fp)
			lv, lok := lookup(o.Live.Object, fp)
			if !gok && !lok && len(fp) > 1 {
				// Unset in both is equal only when the field's parent is
				// there: otherwise the path itself is likely wrong.
				_, gp := lookup(o.Git.Object, fp[:len(fp)-1])
				_, lp := lookup(o.Live.Object, fp[:len(fp)-1])
				if !gp && !lp {
					return fs, fmt.Errorf("%s field %s: the path resolves in neither Git nor live; a key that holds a dot cannot be written in this form",
						refString(o.Ref), names[id][i])
				}
			}
			if gok != lok || (gok && !jsonEqual(gv, lv)) {
				fs.Fields = append(fs.Fields, fmt.Sprintf("%s field %s: Git has %s, live has %s",
					refString(o.Ref), names[id][i], value(gv, gok), value(lv, lok)))
				continue
			}
			for _, d := range drift {
				if overlaps(d, fp) {
					mv, mok := lookup(o.Result.Merged.Object, fp)
					av, aok := lookup(o.Result.Live.Object, fp)
					fs.Fields = append(fs.Fields, fmt.Sprintf("%s field %s: applying Git changes %s: live has %s, after apply %s",
						refString(o.Ref), names[id][i], d, value(av, aok), value(mv, mok)))
					break
				}
			}
		}
		var other []string
		for _, d := range drift {
			mine := false
			for _, fp := range byObj[id] {
				mine = mine || overlaps(d, fp)
			}
			if !mine {
				other = append(other, d.String())
			}
		}
		if unknown {
			other = append(other, "(Flux reports a difference that Tack cannot locate)")
		}
		if len(other) > 0 {
			fs.Other = append(fs.Other, fmt.Sprintf("%s: %s", refString(o.Ref), strings.Join(other, ", ")))
		}
	}
	for _, fl := range fields {
		if !seen[refID(fl.Object)] {
			return fs, fmt.Errorf("field %s of %s: the object is not held", fl.Field, refString(fl.Object))
		}
	}
	return fs, nil
}

func verdict(fs findings, n int, site string, strict bool) (string, error) {
	if len(fs.Fields) > 0 {
		return "", stop.Ask(v1alpha1.ReasonGitDiffersFromLive,
			"at site %s, Git differs from live in fields this run changed: %s", site, strings.Join(fs.Fields, "; "))
	}
	if len(fs.Other) > 0 {
		if strict {
			return "", stop.Ask(v1alpha1.ReasonGitDiffersFromLive,
				"at site %s, applying Git would change fields this run did not change: %s", site, strings.Join(fs.Other, "; "))
		}
		return fmt.Sprintf("Git equals live in this run's fields for %d objects; applying Git also changes %s",
			n, strings.Join(fs.Other, "; ")), nil
	}
	return fmt.Sprintf("Git equals live for %d objects", n), nil
}

// driftPaths lists where merged differs from live, as kustomize-controller
// detects drift: labels, annotations, and everything outside metadata and
// status.
func driftPaths(live, merged *unstructured.Unstructured) []path {
	var out []path
	for _, k := range []string{"labels", "annotations"} {
		a, _, _ := unstructured.NestedFieldNoCopy(live.Object, "metadata", k)
		b, _, _ := unstructured.NestedFieldNoCopy(merged.Object, "metadata", k)
		diffPaths(a, b, path{{key: "metadata"}, {key: k}}, &out)
	}
	diffPaths(body(live).Object, body(merged).Object, nil, &out)
	return out
}

// body is a copy of u without metadata and status.
func body(u *unstructured.Unstructured) *unstructured.Unstructured {
	c := u.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata")
	unstructured.RemoveNestedField(c.Object, "status")
	if err := normalize.DryRunUnstructured(c); err != nil {
		return u
	}
	return c
}

// VerifyNoop implements engine.Flux. The ledger keeps each object's
// generation at release, not its field values. So it checks:
//   - the generation is the one at release, which covers every field
//     outside metadata;
//   - the object is not held again;
//   - each owning Kustomization is not failing, and at which revision it is.
//
// A field under metadata, or an object without a recorded generation, is
// not covered; the result says so.
func (f *Flux) VerifyNoop(ctx context.Context, site string, held []v1alpha1.HeldObject, fields []v1alpha1.ObjectField, strict bool) (string, error) {
	if len(held) == 0 {
		return "no object is held at site " + site, nil
	}
	c, err := f.Clients.Client(site)
	if err != nil {
		return "", err
	}
	var changed, notes []string
	unchecked := 0
	moved := map[objID]bool{}
	known := map[objID]bool{}
	var order []v1alpha1.ObjectKey
	seenKs := map[v1alpha1.ObjectKey]bool{}
	for _, h := range held {
		if h.ReleasedAt == nil {
			return "", fmt.Errorf("%s at site %s is not released", refString(h.Object), site)
		}
		u, err := getObject(ctx, c, h.Object)
		if err != nil {
			return "", err
		}
		if isHeld(u) {
			return "", stop.Ask(v1alpha1.ReasonHeldByAnotherRun,
				"%s at site %s is held again after release: Flux does not apply it", refString(h.Object), site)
		}
		id := refID(h.Object)
		known[id] = true
		switch {
		case h.Generation == 0:
			unchecked++
			notes = append(notes, fmt.Sprintf("%s has no generation recorded at release: not checked", refString(h.Object)))
		case u.GetGeneration() != h.Generation:
			changed = append(changed, fmt.Sprintf("%s generation %d at release, now %d", refString(h.Object), h.Generation, u.GetGeneration()))
			moved[id] = true
		}
		if k := h.Kustomization; k.Name != "" && !seenKs[k] {
			seenKs[k] = true
			order = append(order, k)
		}
	}
	for _, fl := range fields {
		id := refID(fl.Object)
		if !known[id] {
			return "", fmt.Errorf("field %s of %s: the object is not held at site %s", fl.Field, refString(fl.Object), site)
		}
		p, err := parsePath(fl.Field)
		if err != nil {
			return "", fmt.Errorf("%s: %w", refString(fl.Object), err)
		}
		if p[0].key == "metadata" {
			notes = append(notes, fmt.Sprintf("%s field %s is metadata, which the generation does not cover: not checked", refString(fl.Object), fl.Field))
			continue
		}
		if moved[id] {
			notes = append(notes, fmt.Sprintf("%s field %s may have changed", refString(fl.Object), fl.Field))
		}
	}
	var revs []string
	for _, k := range order {
		u, err := getKind(ctx, c, KustomizationGVK, k)
		if err != nil {
			return "", err
		}
		st, reason, msg := ready(u)
		switch {
		case st == "False":
			return "", fmt.Errorf("Kustomization %s at site %s is failing after release: %s: %s", keyString(k), site, reason, msg)
		case st == "True" && reason == ReasonSucceeded:
			revs = append(revs, keyString(k)+" at "+str(u, "status", "lastAppliedRevision"))
		default:
			notes = append(notes, fmt.Sprintf("Kustomization %s is reconciling (Ready=%s %s)", keyString(k), st, reason))
		}
	}
	if len(changed) > 0 && strict {
		return "", stop.Ask(v1alpha1.ReasonFluxChanged, "at site %s, objects changed after release: %s", site, strings.Join(changed, "; "))
	}
	msg := fmt.Sprintf("Flux changed nothing in %d objects", len(held)-len(changed)-unchecked)
	if len(revs) > 0 {
		msg += " (Kustomization " + strings.Join(revs, ", ") + ")"
	}
	if len(changed) > 0 {
		msg += "; changed after release, as the accepted Git commit allows: " + strings.Join(changed, "; ")
	}
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	return msg, nil
}
