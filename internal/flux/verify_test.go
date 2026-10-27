package flux

import (
	"context"
	"strings"
	"testing"

	"github.com/fluxcd/pkg/ssa"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
)

func TestParsePath(t *testing.T) {
	p, err := parsePath("spec.containers[0].env[2].value")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "spec.containers[0].env[2].value" {
		t.Errorf("round trip %q", got)
	}
	for _, bad := range []string{"", "spec..a", "spec.a[x]", "spec.a[1", "[0]"} {
		if _, err := parsePath(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	obj := map[string]any{"spec": map[string]any{"l": []any{map[string]any{"v": "x"}}}}
	if v, ok := lookup(obj, mustPath("spec.l[0].v")); !ok || v != "x" {
		t.Errorf("lookup: %v %v", v, ok)
	}
	if _, ok := lookup(obj, mustPath("spec.l[1].v")); ok {
		t.Error("lookup past the end")
	}
}

func mustPath(s string) path {
	p, err := parsePath(s)
	if err != nil {
		panic(err)
	}
	return p
}

func TestOverlaps(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"spec.replica", "spec.replica.primary", true},
		{"spec.replica.primary", "spec.replica", true},
		{"spec.replica.primary", "spec.replica.primary", true},
		{"spec.replica.primary", "spec.replica.source", false},
		{"spec.l[0]", "spec.l[1]", false},
		{"spec.rep", "spec.replica", false},
	} {
		if got := overlaps(mustPath(c.a), mustPath(c.b)); got != c.want {
			t.Errorf("overlaps(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestDriftPaths(t *testing.T) {
	live := object(clusterGVK, "db", "pg", map[string]any{
		"spec":   map[string]any{"instances": int64(3), "l": []any{"a", "b"}, "empty": map[string]any{}},
		"status": map[string]any{"phase": "x"},
	})
	live.SetLabels(map[string]string{"a": "1"})
	merged := object(clusterGVK, "db", "pg", map[string]any{
		"spec":   map[string]any{"instances": float64(3), "l": []any{"a", "c"}, "new": "v"},
		"status": map[string]any{"phase": "y"},
	})
	merged.SetLabels(map[string]string{"a": "2"})
	merged.SetResourceVersion("9")
	var got []string
	for _, p := range driftPaths(live, merged) {
		got = append(got, p.String())
	}
	if want := "metadata.labels.a,spec.l[1],spec.new"; strings.Join(got, ",") != want {
		t.Errorf("drift %v, want %s", got, want)
	}
}

func clusterWith(primary string, instances int64, description string) *unstructured.Unstructured {
	u := object(clusterGVK, "db", "pg", map[string]any{
		"spec": map[string]any{
			"instances":   instances,
			"description": description,
			"replica":     map[string]any{"primary": primary, "source": "pg-a"},
		},
	})
	return u
}

var (
	primaryField = v1alpha1.ObjectField{Object: clusterRef, Field: "spec.replica.primary"}
	secretGVK    = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
)

func TestClassify(t *testing.T) {
	git := clusterWith("pg-b", 3, "d")
	mask := &masker{}
	mask.add(map[string]string{"token": secret})

	t.Run("equal", func(t *testing.T) {
		fs, err := classify([]checked{{Ref: clusterRef, Git: git, Live: git.DeepCopy(), Result: diffResult{Action: ssa.UnchangedAction}}},
			[]v1alpha1.ObjectField{primaryField}, mask)
		if err != nil || len(fs.Fields)+len(fs.Other) != 0 {
			t.Fatalf("%+v %v", fs, err)
		}
		msg, err := verdict(fs, 1, "a", true)
		if err != nil || msg != "Git equals live for 1 objects" {
			t.Fatalf("%q %v", msg, err)
		}
	})
	t.Run("field differs", func(t *testing.T) {
		live := clusterWith("pg-a", 3, "d")
		fs, err := classify([]checked{{Ref: clusterRef, Git: git, Live: live,
			Result: diffResult{Action: ssa.ConfiguredAction, Live: live, Merged: git}}},
			[]v1alpha1.ObjectField{primaryField}, mask)
		if err != nil {
			t.Fatal(err)
		}
		if len(fs.Fields) != 1 || !strings.Contains(fs.Fields[0], `spec.replica.primary: Git has "pg-b", live has "pg-a"`) || len(fs.Other) != 0 {
			t.Fatalf("%+v", fs)
		}
		for _, strict := range []bool{true, false} {
			_, err = verdict(fs, 1, "a", strict)
			wantStop(t, err, v1alpha1.ReasonGitDiffersFromLive, "spec.replica.primary")
		}
	})
	t.Run("field missing from Git", func(t *testing.T) {
		g := git.DeepCopy()
		unstructured.RemoveNestedField(g.Object, "spec", "replica", "primary")
		fs, _ := classify([]checked{{Ref: clusterRef, Git: g, Live: git, Result: diffResult{Action: ssa.UnchangedAction}}},
			[]v1alpha1.ObjectField{primaryField}, mask)
		if len(fs.Fields) != 1 || !strings.Contains(fs.Fields[0], "Git has (unset)") {
			t.Fatalf("%+v", fs)
		}
	})
	t.Run("field removed in both", func(t *testing.T) {
		g := git.DeepCopy()
		unstructured.RemoveNestedField(g.Object, "spec", "replica", "primary")
		fs, _ := classify([]checked{{Ref: clusterRef, Git: g, Live: g.DeepCopy(), Result: diffResult{Action: ssa.UnchangedAction}}},
			[]v1alpha1.ObjectField{primaryField}, mask)
		if len(fs.Fields) != 0 {
			t.Fatalf("%+v", fs)
		}
	})
	t.Run("path that resolves nowhere", func(t *testing.T) {
		g := git.DeepCopy()
		g.SetAnnotations(map[string]string{"cnpg.io/hibernation": "off"})
		for _, field := range []string{"spec.replcia.primary", "metadata.annotations.cnpg.io/hibernation"} {
			_, err := classify([]checked{{Ref: clusterRef, Git: g, Live: g.DeepCopy(), Result: diffResult{Action: ssa.UnchangedAction}}},
				[]v1alpha1.ObjectField{{Object: clusterRef, Field: field}}, mask)
			if err == nil || !strings.Contains(err.Error(), "resolves in neither") {
				t.Errorf("%s: got %v", field, err)
			}
		}
	})
	t.Run("apply changes the field below it", func(t *testing.T) {
		live := clusterWith("pg-b", 3, "d")
		merged := clusterWith("pg-b", 3, "d")
		_ = unstructured.SetNestedField(merged.Object, "x", "spec", "replica", "primary")
		field := v1alpha1.ObjectField{Object: clusterRef, Field: "spec.replica"}
		fs, _ := classify([]checked{{Ref: clusterRef, Git: live, Live: live,
			Result: diffResult{Action: ssa.ConfiguredAction, Live: live, Merged: merged}}},
			[]v1alpha1.ObjectField{field}, mask)
		if len(fs.Fields) != 1 || !strings.Contains(fs.Fields[0], "applying Git changes spec.replica.primary") || len(fs.Other) != 0 {
			t.Fatalf("%+v", fs)
		}
	})
	t.Run("apply changes the field above it", func(t *testing.T) {
		live := clusterWith("pg-b", 3, "d")
		merged := live.DeepCopy()
		unstructured.RemoveNestedField(merged.Object, "spec", "replica")
		fs, _ := classify([]checked{{Ref: clusterRef, Git: live, Live: live,
			Result: diffResult{Action: ssa.ConfiguredAction, Live: live, Merged: merged}}},
			[]v1alpha1.ObjectField{primaryField}, mask)
		if len(fs.Fields) != 1 || !strings.Contains(fs.Fields[0], "applying Git changes spec.replica:") {
			t.Fatalf("%+v", fs)
		}
	})
	t.Run("other paths", func(t *testing.T) {
		live := clusterWith("pg-b", 2, "token old")
		merged := clusterWith("pg-b", 3, "token "+secret)
		fs, err := classify([]checked{{Ref: clusterRef, Git: merged, Live: live,
			Result: diffResult{Action: ssa.ConfiguredAction, Live: live, Merged: merged}}},
			[]v1alpha1.ObjectField{primaryField}, mask)
		if err != nil {
			t.Fatal(err)
		}
		if len(fs.Fields) != 0 || len(fs.Other) != 1 || fs.Other[0] != "Cluster db/pg: spec.description, spec.instances" {
			t.Fatalf("%+v", fs)
		}
		_, err = verdict(fs, 1, "a", true)
		wantStop(t, err, v1alpha1.ReasonGitDiffersFromLive, "spec.description, spec.instances")
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "token old") {
			t.Errorf("values in %q", err)
		}
		msg, err := verdict(fs, 1, "a", false)
		if err != nil || !strings.Contains(msg, "spec.instances") {
			t.Fatalf("%q %v", msg, err)
		}
	})
	t.Run("secret in a field value", func(t *testing.T) {
		live := clusterWith("pg-b", 3, "token old")
		g := clusterWith("pg-b", 3, "token "+secret)
		field := v1alpha1.ObjectField{Object: clusterRef, Field: "spec.description"}
		fs, _ := classify([]checked{{Ref: clusterRef, Git: g, Live: live,
			Result: diffResult{Action: ssa.ConfiguredAction, Live: live, Merged: g}}},
			[]v1alpha1.ObjectField{field}, mask)
		if len(fs.Fields) != 1 || strings.Contains(fs.Fields[0], secret) || !strings.Contains(fs.Fields[0], `"token *****"`) {
			t.Fatalf("%+v", fs)
		}
	})
	t.Run("Secret field values stay hidden", func(t *testing.T) {
		ref := v1alpha1.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: "db", Name: "s"}
		g := object(secretGVK, "db", "s", map[string]any{"data": map[string]any{"k": "bmV3"}})
		live := object(secretGVK, "db", "s", map[string]any{"data": map[string]any{"k": "b2xk"}})
		fs, _ := classify([]checked{{Ref: ref, Git: g, Live: live, Result: diffResult{Action: ssa.UnchangedAction}}},
			[]v1alpha1.ObjectField{{Object: ref, Field: "data.k"}}, mask)
		if len(fs.Fields) != 1 || strings.Contains(fs.Fields[0], "bmV3") || strings.Contains(fs.Fields[0], "b2xk") {
			t.Fatalf("%+v", fs)
		}
	})
	t.Run("drift Tack cannot locate", func(t *testing.T) {
		fs, _ := classify([]checked{{Ref: clusterRef, Git: git, Live: git,
			Result: diffResult{Action: ssa.ConfiguredAction, Live: git, Merged: git.DeepCopy()}}}, nil, mask)
		if len(fs.Other) != 1 || !strings.Contains(fs.Other[0], "cannot locate") {
			t.Fatalf("%+v", fs)
		}
	})
	t.Run("created", func(t *testing.T) {
		_, err := classify([]checked{{Ref: clusterRef, Git: git, Live: git, Result: diffResult{Action: ssa.CreatedAction}}}, nil, mask)
		wantStop(t, err, v1alpha1.ReasonGitDiffersFromLive, "create")
	})
	t.Run("field of an object not held", func(t *testing.T) {
		field := v1alpha1.ObjectField{Object: poolerRef, Field: "spec.instances"}
		_, err := classify([]checked{{Ref: clusterRef, Git: git, Live: git, Result: diffResult{Action: ssa.UnchangedAction}}},
			[]v1alpha1.ObjectField{field}, mask)
		if err == nil || !strings.Contains(err.Error(), "not held") {
			t.Fatalf("got %v", err)
		}
	})
}

// mergeDiffer stands in for a server-side dry-run: it lays the Git object
// over live, so live fields Git does not set stay.
type mergeDiffer struct{}

func (mergeDiffer) Diff(ctx context.Context, c client.Client, obj *unstructured.Unstructured, _ ssa.DiffOptions) (diffResult, error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
		return diffResult{}, err
	}
	merged := live.DeepCopy()
	mergeInto(merged.Object, obj.Object)
	if len(driftPaths(live, merged)) == 0 {
		return diffResult{Action: ssa.UnchangedAction}, nil
	}
	return diffResult{Action: ssa.ConfiguredAction, Live: live, Merged: merged}, nil
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		sm, ok1 := v.(map[string]any)
		dm, ok2 := dst[k].(map[string]any)
		if ok1 && ok2 {
			mergeInto(dm, sm)
			continue
		}
		if v != nil {
			dst[k] = v
		}
	}
}

func TestVerifyGit(t *testing.T) {
	ctx := context.Background()
	ks := renderKustomization("./clusters/a")
	r, err := render(ctx, renderClient(), ks, repoDir, []v1alpha1.ObjectRef{clusterRef, poolerRef})
	if err != nil {
		t.Fatal(err)
	}
	// liveFrom returns the rendered objects as live, held by run-1, after
	// edit changes the Cluster.
	liveFrom := func(edit func(u *unstructured.Unstructured)) []client.Object {
		var out []client.Object
		for _, o := range r.Objects {
			u := o.DeepCopy()
			unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
			ann := u.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann[AnnotationReconcile], ann[AnnotationHeldBy] = ReconcileDisabled, "run-1"
			u.SetAnnotations(ann)
			if u.GetKind() == "Cluster" && edit != nil {
				edit(u)
			}
			out = append(out, u)
		}
		return out
	}
	held := []v1alpha1.HeldObject{
		{Site: "a", Object: clusterRef, Kustomization: ksKey},
		{Site: "a", Object: poolerRef, Kustomization: ksKey},
	}
	fields := []v1alpha1.ObjectField{primaryField, {Object: poolerRef, Field: "spec.instances"}}
	verify := func(strict bool, edit func(u *unstructured.Unstructured), h []v1alpha1.HeldObject) (string, error) {
		c := renderClient(append(liveFrom(edit), ks)...)
		f := New(fakeClients{c})
		f.differ = mergeDiffer{}
		return f.VerifyGit(ctx, "a", h, fields, repoDir, strict)
	}

	msg, err := verify(true, nil, held)
	if err != nil || msg != "Git equals live for 2 objects" {
		t.Fatalf("%q %v", msg, err)
	}
	_, err = verify(true, func(u *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(u.Object, "pg-a", "spec", "replica", "primary")
	}, held)
	wantStop(t, err, v1alpha1.ReasonGitDiffersFromLive, `Git has "pg-b", live has "pg-a"`)

	other := func(u *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(u.Object, "token old", "spec", "description")
	}
	_, err = verify(true, other, held)
	wantStop(t, err, v1alpha1.ReasonGitDiffersFromLive, "Cluster db/pg: spec.description")
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Secret value in %q", err)
	}
	msg, err = verify(false, other, held)
	if err != nil || !strings.Contains(msg, "spec.description") {
		t.Fatalf("%q %v", msg, err)
	}

	missing := append(held[:1:1], v1alpha1.HeldObject{Site: "a", Object: v1alpha1.ObjectRef{
		APIVersion: "postgresql.cnpg.io/v1", Kind: "Pooler", Namespace: "db", Name: "gone"}, Kustomization: ksKey})
	_, err = verify(true, nil, missing)
	wantStop(t, err, v1alpha1.ReasonGitDiffersFromLive, "lacks Pooler db/gone")
}

func TestDiffOptions(t *testing.T) {
	ks := renderKustomization("./clusters/a")
	_ = unstructured.SetNestedField(ks.Object, true, "spec", "force")
	_ = unstructured.SetNestedSlice(ks.Object, []any{
		map[string]any{"paths": []any{"/spec/instances"}, "target": map[string]any{"kind": "Pooler", "annotationSelector": "a=b"}},
		map[string]any{"paths": []any{"/spec/x"}},
	}, "spec", "ignore")
	opts, err := diffOptions(ks)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Force || len(opts.Exclusions) != 0 || len(opts.DriftIgnoreRules) != 2 {
		t.Fatalf("%+v", opts)
	}
	r := opts.DriftIgnoreRules[0]
	if r.Paths[0] != "/spec/instances" || r.Selector == nil || r.Selector.Kind != "Pooler" || r.Selector.AnnotationSelector != "a=b" {
		t.Errorf("rule %+v %+v", r, r.Selector)
	}
	if opts.DriftIgnoreRules[1].Selector != nil {
		t.Error("rule without target has a selector")
	}
}

func TestVerifyNoop(t *testing.T) {
	ctx := context.Background()
	released := metav1.Now()
	held := []v1alpha1.HeldObject{{Site: "a", Object: clusterRef, Kustomization: ksKey, ReleasedAt: &released, Generation: 4}}
	fields := []v1alpha1.ObjectField{primaryField}
	ready := func(status, reason string) *unstructured.Unstructured {
		u := kustomization(ksKey, false)
		_ = unstructured.SetNestedSlice(u.Object, readyCond(status, reason, "msg"), "status", "conditions")
		return u
	}
	run := func(strict bool, ks *unstructured.Unstructured, live *unstructured.Unstructured, h []v1alpha1.HeldObject, f []v1alpha1.ObjectField) (string, error) {
		return New(fakeClients{newClient(nil, ks, live)}).VerifyNoop(ctx, "a", h, f, strict)
	}

	msg, err := run(true, ready("True", ReasonSucceeded), liveCluster(nil), held, fields)
	if err != nil || !strings.Contains(msg, "Flux changed nothing in 1 objects") || !strings.Contains(msg, oldRev) {
		t.Fatalf("%q %v", msg, err)
	}

	moved := liveCluster(nil)
	moved.SetGeneration(5)
	_, err = run(true, ready("True", ReasonSucceeded), moved, held, fields)
	wantStop(t, err, v1alpha1.ReasonFluxChanged, "generation 4 at release, now 5")
	msg, err = run(false, ready("True", ReasonSucceeded), moved, held, fields)
	if err != nil || !strings.Contains(msg, "generation 4 at release, now 5") || !strings.Contains(msg, "spec.replica.primary may have changed") {
		t.Fatalf("%q %v", msg, err)
	}

	_, err = run(true, ready("True", ReasonSucceeded), liveCluster(map[string]string{AnnotationReconcile: "disabled"}), held, fields)
	wantStop(t, err, v1alpha1.ReasonHeldByAnotherRun, "held again")

	if _, err = run(true, ready("False", "ReconciliationFailed"), liveCluster(nil), held, fields); err == nil || !strings.Contains(err.Error(), "ReconciliationFailed") {
		t.Fatalf("got %v", err)
	}

	noGen := []v1alpha1.HeldObject{held[0]}
	noGen[0].Generation = 0
	meta := []v1alpha1.ObjectField{{Object: clusterRef, Field: "metadata.labels.role"}}
	msg, err = run(true, ready("True", ReasonSucceeded), liveCluster(nil), noGen, meta)
	if err != nil || !strings.Contains(msg, "no generation recorded") || !strings.Contains(msg, "is metadata") {
		t.Fatalf("%q %v", msg, err)
	}

	notReleased := []v1alpha1.HeldObject{held[0]}
	notReleased[0].ReleasedAt = nil
	if _, err = run(true, ready("True", ReasonSucceeded), liveCluster(nil), notReleased, fields); err == nil {
		t.Fatal("want an error for an object not released")
	}
}
