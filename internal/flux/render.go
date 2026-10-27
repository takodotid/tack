package flux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	generator "github.com/fluxcd/pkg/kustomize"
	"github.com/fluxcd/pkg/sourceignore"
	"github.com/fluxcd/pkg/ssa"
	"github.com/fluxcd/pkg/ssa/normalize"
	ssautil "github.com/fluxcd/pkg/ssa/utils"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
)

// controllerOwner is how kustomize-controller applies and labels objects.
var controllerOwner = ssa.Owner{Field: ControllerFieldManager, Group: KustomizationGVK.Group}

// objID identifies an object across API versions.
type objID struct {
	Group, Kind, Namespace, Name string
}

func refID(r v1alpha1.ObjectRef) objID {
	gv, _ := schema.ParseGroupVersion(r.APIVersion)
	return objID{Group: gv.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}
}

func unstructuredID(u *unstructured.Unstructured) objID {
	return objID{Group: u.GroupVersionKind().Group, Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
}

// masker hides the values of substitution variables that come from Secrets.
type masker struct{ values []string }

func (m *masker) add(vars map[string]string) {
	for _, v := range vars {
		if v != "" {
			m.values = append(m.values, v)
		}
	}
	m.sort()
}

func (m *masker) merge(o *masker) {
	if o != nil {
		m.values = append(m.values, o.values...)
		m.sort()
	}
}

// sort puts longer values first, so a value that contains another is
// hidden whole.
func (m *masker) sort() {
	sort.Slice(m.values, func(i, j int) bool { return len(m.values[i]) > len(m.values[j]) })
}

func (m *masker) mask(s string) string {
	if m == nil {
		return s
	}
	for _, v := range m.values {
		s = strings.ReplaceAll(s, v, "*****")
	}
	return s
}

func (m *masker) err(err error) error {
	if err == nil || m == nil || len(m.values) == 0 {
		return err
	}
	return fmt.Errorf("%s", m.mask(err.Error()))
}

// rendered is the result of building a Kustomization.
type rendered struct {
	// Objects are the requested objects, as kustomize-controller would
	// apply them.
	Objects map[objID]*unstructured.Unstructured
	// Mask hides substituted Secret values.
	Mask *masker
}

// render builds the Kustomization ks from the repository checked out at
// dir, as kustomize-controller would, and returns the objects named by
// refs that the build produces. It reads ks's GitRepository to copy only
// what source-controller puts in its artifact, and reads the ConfigMaps
// and Secrets of spec.postBuild.substituteFrom. It never writes to dir.
// It does not decrypt SOPS: an encrypted object renders encrypted.
func render(ctx context.Context, c client.Client, ks *unstructured.Unstructured, dir string, refs []v1alpha1.ObjectRef) (*rendered, error) {
	name := fmt.Sprintf("Kustomization %s/%s", ks.GetNamespace(), ks.GetName())
	kind, src := sourceRef(ks)
	if kind != GitRepositoryGVK.Kind {
		return nil, fmt.Errorf("%s applies from %s %s, not from a GitRepository: Tack renders only from Git", name, kind, keyString(src))
	}
	repo, err := getKind(ctx, c, GitRepositoryGVK, src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if inc, _, _ := unstructured.NestedSlice(repo.Object, "spec", "include"); len(inc) > 0 {
		return nil, fmt.Errorf("%s: GitRepository %s includes other repositories, which Tack cannot render", name, keyString(src))
	}
	ignore, hasIgnore, _ := unstructured.NestedString(repo.Object, "spec", "ignore")

	root, err := os.MkdirTemp("", "tack-render-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	if err := copySource(dir, root, ignore, hasIgnore); err != nil {
		return nil, fmt.Errorf("%s: copy the checkout: %w", name, err)
	}
	ksPath := str(ks, "spec", "path")
	dirPath := filepath.Join(root, filepath.Clean(string(filepath.Separator)+ksPath))
	if _, err := os.Stat(dirPath); err != nil {
		return nil, fmt.Errorf("%s: path %q is not in the checkout", name, ksPath)
	}
	if _, err := generator.NewGenerator(root, *ks).WriteFile(dirPath); err != nil {
		return nil, fmt.Errorf("%s: generate kustomization.yaml: %w", name, err)
	}
	m, err := generator.SecureBuild(root, dirPath, false)
	if err != nil {
		return nil, fmt.Errorf("%s: kustomize build of %q failed; Tack builds without remote bases: %w", name, ksPath, err)
	}

	mask := &masker{}
	if _, ok, _ := unstructured.NestedMap(ks.Object, "spec", "postBuild"); ok {
		subst, always, err := substitution(ctx, c, ks, mask)
		if err != nil {
			return nil, mask.err(fmt.Errorf("%s: post build: %w", name, err))
		}
		for _, res := range m.Resources() {
			out, err := generator.SubstituteVariables(ctx, c, *subst, res,
				generator.SubstituteWithDryRun(true),
				generator.SubstituteWithStrict(true),
				generator.SubstituteWithAlways(always))
			if err != nil {
				return nil, mask.err(fmt.Errorf("%s: post build of %s %s: %w", name, res.GetKind(), res.GetName(), err))
			}
			if out != nil {
				if _, err := m.Replace(res); err != nil {
					return nil, mask.err(fmt.Errorf("%s: post build of %s %s: %w", name, res.GetKind(), res.GetName(), err))
				}
			}
		}
	}
	for _, res := range m.Resources() {
		if res.GetName() == "" || res.GetKind() == "" || res.GetApiVersion() == "" {
			return nil, fmt.Errorf("%s: the build has an object without apiVersion, kind or name", name)
		}
	}
	y, err := m.AsYaml()
	if err != nil {
		return nil, mask.err(fmt.Errorf("%s: kustomize build: %w", name, err))
	}
	objs, err := ssautil.ReadObjects(bytes.NewReader(y))
	if err != nil {
		return nil, mask.err(fmt.Errorf("%s: read the build: %w", name, err))
	}

	want := map[objID]bool{}
	for _, r := range refs {
		want[refID(r)] = true
	}
	var picked []*unstructured.Unstructured
	for _, o := range objs {
		if want[unstructuredID(o)] {
			picked = append(picked, o)
		}
	}
	ssa.NewResourceManager(nil, nil, controllerOwner).SetOwnerLabels(picked, ks.GetName(), ks.GetNamespace())
	if err := normalize.UnstructuredList(picked); err != nil {
		return nil, mask.err(fmt.Errorf("%s: %w", name, err))
	}
	if cm, ok, _ := unstructured.NestedMap(ks.Object, "spec", "commonMetadata"); ok {
		labels, _, _ := unstructured.NestedStringMap(cm, "labels")
		annotations, _, _ := unstructured.NestedStringMap(cm, "annotations")
		ssautil.SetCommonMetadata(picked, labels, annotations)
	}
	out := &rendered{Objects: map[objID]*unstructured.Unstructured{}, Mask: mask}
	for _, o := range picked {
		out.Objects[unstructuredID(o)] = o
	}
	return out, nil
}

// substitution loads the Kustomization's variables once, and returns a copy
// of ks that carries them all inline, so each resource is substituted
// without reading the cluster again. Values from Secrets go to mask.
func substitution(ctx context.Context, c client.Client, ks *unstructured.Unstructured, mask *masker) (*unstructured.Unstructured, bool, error) {
	ctx = generator.ContextWithSecretVarsCollector(ctx, mask.add)
	vars, err := generator.LoadVariables(ctx, c, *ks)
	if err != nil {
		return nil, false, err
	}
	inline, _, err := unstructured.NestedStringMap(ks.Object, "spec", "postBuild", "substitute")
	if err != nil {
		return nil, false, err
	}
	for k, v := range inline {
		vars[k] = strings.ReplaceAll(v, "\n", "")
	}
	cp := ks.DeepCopy()
	all := map[string]any{}
	for k, v := range vars {
		all[k] = v
	}
	if err := unstructured.SetNestedMap(cp.Object, all, "spec", "postBuild", "substitute"); err != nil {
		return nil, false, err
	}
	always := str(ks, "spec", "postBuild", "substituteStrategy") == "Always"
	return cp, always, nil
}

// copySource copies the checkout at src to dst as source-controller
// archives it: regular files only, without the files its ignore rules
// match. ignore is the GitRepository's spec.ignore.
func copySource(src, dst, ignore string, hasIgnore bool) error {
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	domain := strings.Split(src, string(filepath.Separator))
	ps, err := sourceignore.LoadIgnorePatterns(src, domain)
	if err != nil {
		return err
	}
	if hasIgnore {
		ps = append(ps, sourceignore.ReadPatterns(strings.NewReader(ignore), domain)...)
	}
	matcher := sourceignore.NewDefaultMatcher(ps, domain)
	if len(ps) > 0 {
		matcher = sourceignore.NewMatcher(append(sourceignore.VCSPatterns(domain), ps...))
	}
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Name() == ".git" {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !fi.Mode().IsRegular() || matcher.Match(strings.Split(p, string(filepath.Separator)), false) {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
