package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
)

// Resolve finds where each changed field lives in the repository, in the
// order of changes. A spec.git.fields entry wins; else, with spec.git.find
// set, the manifest that defines the object is searched. When git cannot
// reach the repository, Resolve returns the changes with every mapped one
// resolved and the searched ones without a file, and an error that wraps
// ErrUnreachable. Any other error returns no changes.
func (r *Repo) Resolve(ctx context.Context, w *failoverv1.WorkloadSpec, changes []driver.FieldChange) ([]failoverv1.GitChange, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := &w.Git
	out := make([]failoverv1.GitChange, len(changes))
	mapped := make([]bool, len(changes))
	for i, c := range changes {
		out[i] = failoverv1.GitChange{Site: c.Site, Object: c.Object, Field: c.Field}
		if c.Value != nil {
			v := *c.Value
			out[i].Value = &v
			if !strings.Contains(v, "$(") {
				if _, _, err := renderJSON(v); err != nil {
					return nil, fmt.Errorf("%s: %w", describeChange(&out[i]), err)
				}
			}
		}
		m, err := mapping(g.Fields, c)
		if err != nil {
			return nil, err
		}
		switch {
		case m != nil:
			if out[i].File, err = repoPath(m.File); err != nil {
				return nil, fmt.Errorf("spec.git.fields entry for %s: file %w", describeChange(&out[i]), err)
			}
			if _, err := parsePath(m.Path); err != nil {
				return nil, fmt.Errorf("spec.git.fields entry for %s: %w", describeChange(&out[i]), err)
			}
			out[i].Path = m.Path
			if m.Document != nil {
				d := *m.Document
				out[i].Document = &d
			}
			mapped[i] = true
		case g.Find != nil:
			if _, err := parsePath(c.Field); err != nil {
				return nil, fmt.Errorf("%s: %w", describeChange(&out[i]), err)
			}
			out[i].Path = c.Field
		default:
			return nil, fmt.Errorf("%w: %s: add an entry to spec.git.fields, or set spec.git.find", ErrNoMapping, describeChange(&out[i]))
		}
	}
	if g.Find != nil {
		for _, p := range g.Find.Paths {
			if _, err := repoPath(p.Path); err != nil {
				return nil, fmt.Errorf("spec.git.find.paths for site %s: %w", p.Site, err)
			}
		}
	}
	if len(changes) == 0 {
		return out, nil
	}

	c, err := r.clone(ctx, g)
	if err != nil {
		return nil, err
	}
	head, err := r.fetch(ctx, c)
	if errors.Is(err, ErrUnreachable) {
		return out, err
	}
	if err != nil {
		return nil, err
	}
	if err := r.reset(ctx, c, head); err != nil {
		return nil, err
	}
	f := r.newFinder(c, head, g.Find)
	for i := range out {
		if !mapped[i] {
			if err := f.resolve(ctx, &out[i]); err != nil {
				return nil, err
			}
		}
	}
	for i := range out {
		ch := &out[i]
		data, err := r.readFile(ctx, c, head, ch.File)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", describeChange(ch), err)
		}
		if err := checkField(data, ch.Document, ch.Path, ch.Value == nil, mapped[i]); err != nil {
			return nil, fmt.Errorf("%s: %s%s: %w", describeChange(ch), ch.File, docString(ch.Document), err)
		}
	}
	return out, nil
}

// mapping returns the spec.git.fields entry of a change, or nil. The entry
// matches on site, kind, name, field, and namespace when it sets one.
func mapping(fields []failoverv1.FieldMapping, c driver.FieldChange) (*failoverv1.FieldMapping, error) {
	var found []int
	for i, m := range fields {
		if m.Site == c.Site && m.Object.Kind == c.Object.Kind && m.Object.Name == c.Object.Name &&
			(m.Object.Namespace == "" || m.Object.Namespace == c.Object.Namespace) && m.Field == c.Field {
			found = append(found, i)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &fields[found[0]], nil
	}
	return nil, fmt.Errorf("%s: spec.git.fields entries %v all match: keep one", describe(c.Site, c.Object, c.Field), found)
}

// manifest is a YAML document that defines a Kubernetes object.
type manifest struct {
	file      string
	doc       int
	group     string
	kind      string
	name      string
	namespace string
	// namespaced is false when the document sets no metadata.namespace.
	namespaced bool
}

// index holds the manifests of one site's search paths.
type index struct {
	where     string
	manifests []manifest
	skipped   []string
}

// finder searches the manifests of a commit that the clone's working tree holds.
type finder struct {
	r      *Repo
	c      *clone
	commit string
	spec   *failoverv1.FindSpec
	sites  map[string]*index
}

func (r *Repo) newFinder(c *clone, commit string, spec *failoverv1.FindSpec) *finder {
	return &finder{r: r, c: c, commit: commit, spec: spec, sites: map[string]*index{}}
}

// resolve sets the file, path and document of a change to the one manifest
// that defines its object.
func (f *finder) resolve(ctx context.Context, ch *failoverv1.GitChange) error {
	if f.spec == nil {
		return fmt.Errorf("%w: %s: add an entry to spec.git.fields, or set spec.git.find", ErrNoMapping, describeChange(ch))
	}
	ix, err := f.index(ctx, ch.Site)
	if err != nil {
		return err
	}
	obj := ch.Object
	group := groupOf(obj.APIVersion)
	var cands []manifest
	for _, m := range ix.manifests {
		if m.group == group && m.kind == obj.Kind && m.name == obj.Name && (!m.namespaced || m.namespace == obj.Namespace) {
			cands = append(cands, m)
		}
	}
	if len(cands) == 1 {
		d := int32(cands[0].doc)
		ch.File, ch.Document = cands[0].file, &d
		if ch.Path == "" {
			ch.Path = ch.Field
		}
		return nil
	}
	hint := "add a spec.git.fields entry for it"
	if len(cands) == 0 {
		msg := fmt.Sprintf("%s: no manifest of the object found in %s", describeChange(ch), ix.where)
		if len(ix.skipped) > 0 {
			msg += fmt.Sprintf(" (skipped as not valid YAML: %s)", listNames(ix.skipped))
		}
		return fmt.Errorf("%s; %s", msg, hint)
	}
	var names []string
	for _, m := range cands {
		names = append(names, fmt.Sprintf("%s document %d", m.file, m.doc))
	}
	return fmt.Errorf("%s: several manifests define the object: %s; %s, or limit spec.git.find.paths",
		describeChange(ch), strings.Join(names, ", "), hint)
}

// index reads the manifests under a site's search paths.
func (f *finder) index(ctx context.Context, site string) (*index, error) {
	if ix, ok := f.sites[site]; ok {
		return ix, nil
	}
	var paths []string
	whole := true
	for _, p := range f.spec.Paths {
		if p.Site != site {
			continue
		}
		clean, err := repoPath(p.Path)
		if err != nil {
			return nil, fmt.Errorf("spec.git.find.paths for site %s: %w", site, err)
		}
		if clean == "." {
			paths, whole = nil, true
			break
		}
		paths, whole = append(paths, clean), false
	}
	ix := &index{where: "the whole repository"}
	if !whole {
		ix.where = strings.Join(paths, ", ")
	}
	files, err := f.r.files(ctx, f.c, f.commit, paths)
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		if ext := strings.ToLower(path.Ext(name)); ext != ".yaml" && ext != ".yml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.c.dir, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		docs, err := documents(data)
		if err != nil {
			ix.skipped = append(ix.skipped, name)
			continue
		}
		for i, d := range docs {
			if m, ok := manifestOf(d); ok {
				m.file, m.doc = name, i
				ix.manifests = append(ix.manifests, m)
			}
		}
	}
	f.sites[site] = ix
	return ix, nil
}

// manifestOf reads the identity of the object a document defines.
func manifestOf(doc *yaml.Node) (manifest, bool) {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return manifest{}, false
	}
	root := doc.Content[0]
	str := func(m *yaml.Node, key string) (string, bool) {
		_, v := lookup(m, key)
		if v == nil || v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			return "", false
		}
		return v.Value, true
	}
	apiVersion, ok1 := str(root, "apiVersion")
	kind, ok2 := str(root, "kind")
	_, meta := lookup(root, "metadata")
	if !ok1 || !ok2 || meta == nil || meta.Kind != yaml.MappingNode {
		return manifest{}, false
	}
	name, ok := str(meta, "name")
	if !ok || name == "" {
		return manifest{}, false
	}
	m := manifest{group: groupOf(apiVersion), kind: kind, name: name}
	m.namespace, m.namespaced = str(meta, "namespace")
	return m, true
}

// groupOf returns the API group of an apiVersion such as apps/v1.
func groupOf(apiVersion string) string {
	if i := strings.LastIndex(apiVersion, "/"); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

func listNames(names []string) string {
	const most = 5
	if len(names) <= most {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:most], ", "), len(names)-most)
}
