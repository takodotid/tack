// Package testenv holds fakes of the sites, Flux and Git for unit tests.
package testenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/stop"
)

// GVKs the fakes know besides the core types.
var GVKs = []schema.GroupVersionKind{
	{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"},
	{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"},
	{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"},
	{Group: "barmancloud.cnpg.io", Version: "v1", Kind: "ObjectStore"},
	{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
	{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"},
	{Group: "strait.tako.id", Version: "v1", Kind: "SiteService"},
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "failover.tako.id", Version: "v1", Kind: "SiteFailover"},
}

// Clients is a fake kube.Clients with one fake client per site.
type Clients struct {
	mu    sync.Mutex
	Sites map[string]client.Client
	Down  map[string]bool
	// Writes counts the writes made through Client.
	Writes int
	// Checks counts the calls to Reachable, by site.
	Checks map[string]int
	// answers holds, by site, the results of the next checks: false fails.
	answers map[string][]bool
	// errs holds, by site, an error every check returns, such as a refused
	// credential.
	errs map[string]error
	// status and statusErr are what the site's pods report (SetStatus).
	status    map[string]map[string]any
	statusErr map[string]error
	proxied   []Proxied
	// faults are the failures the fake CloudNativePG reports (SetFault).
	faults map[string]string
}

// Faults of the fake CloudNativePG: every Backup fails, every WAL archive
// attempt fails.
const (
	FaultBackup  = "backup"
	FaultArchive = "archive"
)

// SetFault makes the fake CloudNativePG fail what kind names with msg, or
// work again with "". The Operator's goroutine reads it under the lock.
func (c *Clients) SetFault(kind, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faults == nil {
		c.faults = map[string]string{}
	}
	c.faults[kind] = msg
}

// Fault returns the failure SetFault set for kind.
func (c *Clients) Fault(kind string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.faults[kind]
}

// NewClients returns fake clients for the sites, seeded with objects. Each
// site's cluster has a kube-system namespace with UID uid-<site>, unless
// objects hold one. Writes through Client are counted in Writes.
func NewClients(objects map[string][]client.Object) *Clients {
	c := &Clients{Sites: map[string]client.Client{}, Down: map[string]bool{}, Checks: map[string]int{}, answers: map[string][]bool{}, errs: map[string]error{}}
	for site, objs := range objects {
		has := false
		for _, o := range objs {
			has = has || o.GetName() == metav1.NamespaceSystem && o.GetNamespace() == "" && o.GetObjectKind().GroupVersionKind().Kind == "Namespace"
		}
		if !has {
			objs = append(objs, KubeSystem("uid-"+site))
		}
		c.Sites[site] = NewClient(objs...)
	}
	return c
}

// KubeSystem returns a kube-system namespace with uid, which fingerprints
// a cluster.
func KubeSystem(uid string) *corev1.Namespace {
	ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}}
	ns.Name, ns.UID = metav1.NamespaceSystem, types.UID(uid)
	return ns
}

// Server implements the optional Server of kube.Clients.
func (c *Clients) Server(site string) (string, error) {
	return "https://" + site + ".example.invalid:6443", nil
}

func (c *Clients) counted(cl client.Client) client.Client {
	count := func() { c.mu.Lock(); c.Writes++; c.mu.Unlock() }
	return interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			count()
			return cl.Create(ctx, o, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			count()
			return cl.Update(ctx, o, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
			count()
			return cl.Patch(ctx, o, p, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			count()
			return cl.Delete(ctx, o, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			count()
			return cl.SubResource(sub).Update(ctx, o, opts...)
		},
	})
}

// User is the user that a fake API server gives the operator's
// credentials, in a SelfSubjectReview.
const User = "operator@example.com"

// NewClient returns a fake client that knows the GVKs, takes Events, and
// answers a SelfSubjectReview with User, as an API server does, without
// storing it. Like an API server, it drops a SiteFailover's status on
// create, in the store and in the object it returns: the status
// subresource ignores it (#38 item 4). Objects seeded with objs keep
// theirs: they stand for ledgers written before.
func NewClient(objs ...client.Object) client.Client {
	objs = withPods(objs)
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range GVKs {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Event"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	mapper.Add(authenticationv1.SchemeGroupVersion.WithKind("SelfSubjectReview"), meta.RESTScopeRoot)
	mapper.Add(coordinationv1.SchemeGroupVersion.WithKind("Lease"), meta.RESTScopeNamespace)
	return fake.NewClientBuilder().
		WithScheme(kube.NewScheme()).
		WithRESTMapper(mapper).
		WithObjects(objs...).
		WithStatusSubresource(&failoverv1.SiteFailover{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if r, ok := o.(*authenticationv1.SelfSubjectReview); ok {
					r.Status.UserInfo.Username = User
					return nil
				}
				if sf, ok := o.(*failoverv1.SiteFailover); ok {
					sf.Status = failoverv1.SiteFailoverStatus{}
				}
				return c.Create(ctx, o, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				if err := unknownPrimary(ctx, c, o, p); err != nil {
					return err
				}
				return c.Patch(ctx, o, p, opts...)
			},
		}).
		Build()
}

// unknownPrimary refuses, as CloudNativePG's webhook does, a patch that
// makes a Cluster declare a primary that its spec.externalClusters do not
// name. The text is the one observed on kind (#66). A Cluster without
// spec.externalClusters is not checked.
func unknownPrimary(ctx context.Context, c client.Client, o client.Object, p client.Patch) error {
	gvk := o.GetObjectKind().GroupVersionKind()
	if gvk.Group != "postgresql.cnpg.io" || gvk.Kind != "Cluster" {
		return nil
	}
	data, err := p.Data(o)
	if err != nil {
		return err
	}
	var patch map[string]any
	if json.Unmarshal(data, &patch) != nil {
		return nil
	}
	primary, _, _ := unstructured.NestedString(patch, "spec", "replica", "primary")
	if primary == "" {
		return nil
	}
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
		return nil
	}
	list, found, _ := unstructured.NestedSlice(cur.Object, "spec", "externalClusters")
	if !found {
		return nil
	}
	for _, e := range list {
		if m, ok := e.(map[string]any); ok && m["name"] == primary {
			return nil
		}
	}
	return apierrors.NewInvalid(gvk.GroupKind(), o.GetName(), field.ErrorList{
		field.Invalid(field.NewPath("spec", "replicaCluster", "primary"), primary, "External cluster "+primary+" not found"),
	})
}

// CreateLedger creates sf with its status, as a run writes a ledger: the
// create, which drops the status, then a status write.
func CreateLedger(ctx context.Context, c client.Client, sf *failoverv1.SiteFailover) error {
	status := sf.Status
	if err := c.Create(ctx, sf); err != nil {
		return err
	}
	sf.Status = status
	return c.Status().Update(ctx, sf)
}

// Events returns the Events recorded on the ledger run at the client c,
// each as "Type Reason: Message", in no particular order.
func Events(c client.Client, namespace, run string) []string {
	var l corev1.EventList
	if err := c.List(context.Background(), &l, client.InNamespace(namespace)); err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, e := range l.Items {
		if e.InvolvedObject.Kind == "SiteFailover" && e.InvolvedObject.Name == run {
			out = append(out, e.Type+" "+e.Reason+": "+e.Message)
		}
	}
	return out
}

// SetSite makes a site's client c, such as one for another cluster.
func (c *Clients) SetSite(site string, cl client.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Sites[site] = cl
}

// SetDown marks a site unreachable or back.
func (c *Clients) SetDown(site string, down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Down[site] = down
}

// Client implements kube.Clients.
func (c *Clients) Client(site string) (client.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Down[site] {
		return nil, fmt.Errorf("site %s is unreachable", site)
	}
	cl, ok := c.Sites[site]
	if !ok {
		return nil, fmt.Errorf("site %q has no fake client", site)
	}
	return c.counted(cl), nil
}

// IsDown tells whether a site is lost. Unlike Reachable, it is neither
// counted nor failed by a blip.
func (c *Clients) IsDown(site string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Down[site]
}

// Blip makes the next n checks of a site fail, as a short network cut
// between the operator's host and the site does.
func (c *Clients) Blip(site string, n int) {
	c.Flap(site, make([]bool, n)...)
}

// Flap sets the results of the next checks of a site: false fails.
func (c *Clients) Flap(site string, answers ...bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers[site] = answers
}

// Fail makes every check of a site return err, as a site whose API
// refuses the operator's credential answers. Nil clears it.
func (c *Clients) Fail(site string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs[site] = err
}

// Reachable implements kube.Clients. A site that is down or blips gives no
// answer: its error is a kube.UnreachableError.
func (c *Clients) Reachable(_ context.Context, site string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Checks[site]++
	if err := c.errs[site]; err != nil {
		return err
	}
	if c.Down[site] {
		return fmt.Errorf("site %s is unreachable: %w", site, &kube.UnreachableError{Err: errors.New("connection refused")})
	}
	if a := c.answers[site]; len(a) > 0 {
		c.answers[site] = a[1:]
		if !a[0] {
			return fmt.Errorf("site %s is unreachable: %w", site, &kube.UnreachableError{Err: errors.New("context deadline exceeded")})
		}
	}
	return nil
}

// Object builds an unstructured object.
func Object(apiVersion, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// Flux is a fake engine.Flux. Every object is applied by Kustomization
// flux-system/<site>, unless Owners names its owner.
type Flux struct {
	mu       sync.Mutex
	Revision string
	// Revisions is the revision Flux applied at a site, by site, when it
	// is not Revision.
	Revisions map[string]string
	// Owners is how Flux applies an object, by <site>/<kind>/<name>.
	Owners map[string]engine.Owner
	Held   map[string]string
	// Generations is an object's generation, by <site>/<kind>/<name>,
	// when it is not 1. VerifyNoop compares it with the one the ledger
	// recorded.
	Generations map[string]int64
	Calls       []string
	// Fail makes a method return an error, by method name.
	Fail map[string]error
	// GitDown makes a Git fetch hang for half the timeout, as a
	// GitRepository does when it cannot reach Git.
	GitDown bool
	// Hung counts the fetches that hung because of GitDown.
	Hung int
}

// NewFlux returns a fake Flux at a revision.
func NewFlux(rev string) *Flux {
	return &Flux{Revision: rev, Held: map[string]string{}, Fail: map[string]error{}}
}

func (f *Flux) call(name string, a ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, name+" "+fmt.Sprint(a...))
	return f.Fail[name]
}

func key(site string, o failoverv1.ObjectRef) string { return site + "/" + o.Kind + "/" + o.Name }

// Owner implements engine.Flux.
func (f *Flux) Owner(_ context.Context, site string, obj failoverv1.ObjectRef) (engine.Owner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.Owners[key(site, obj)]; ok {
		return o, nil
	}
	return engine.Owner{Kustomization: &failoverv1.ObjectKey{Namespace: "flux-system", Name: site}}, nil
}

// Hold implements engine.Flux.
func (f *Flux) Hold(_ context.Context, site string, obj failoverv1.ObjectRef, run string, takeOver []string) (failoverv1.ObjectKey, error) {
	if err := f.call("Hold", site, " ", obj.Name); err != nil {
		return failoverv1.ObjectKey{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.Held[key(site, obj)]; ok && h != run && !slices.Contains(takeOver, h) {
		return failoverv1.ObjectKey{}, stop.Ask(failoverv1.ReasonHeldByAnotherRun, "held by %s", h)
	}
	f.Held[key(site, obj)] = run
	return failoverv1.ObjectKey{Namespace: "flux-system", Name: site}, nil
}

// HeldBy implements engine.Flux.
func (f *Flux) HeldBy(_ context.Context, site string, obj failoverv1.ObjectRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Held[key(site, obj)], nil
}

func (f *Flux) generation(k string) int64 {
	if g, ok := f.Generations[k]; ok {
		return g
	}
	return 1
}

// Release implements engine.Flux.
func (f *Flux) Release(_ context.Context, site string, obj failoverv1.ObjectRef, run string, record func(int64) error) (int64, error) {
	if err := f.call("Release", site, " ", obj.Name); err != nil {
		return 0, err
	}
	k := key(site, obj)
	f.mu.Lock()
	gen, held := f.generation(k), f.Held[k] == run
	f.mu.Unlock()
	if !held {
		return gen, nil
	}
	if record != nil {
		if err := record(gen); err != nil {
			return 0, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Held, k)
	return gen, nil
}

// Reconcile implements engine.Flux. Calls records "fetch" or "no-fetch"
// after the site.
func (f *Flux) Reconcile(ctx context.Context, site string, _ []failoverv1.ObjectKey, revision string, fetchGit bool, timeout time.Duration) (string, []string, error) {
	fetch := "no-fetch"
	if fetchGit {
		fetch = "fetch"
	}
	if err := f.call("Reconcile", site, " ", fetch, " ", revision); err != nil {
		return "", nil, err
	}
	var notes []string
	f.mu.Lock()
	down := f.GitDown
	f.mu.Unlock()
	if fetchGit && down {
		f.mu.Lock()
		f.Hung++
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(timeout / 2):
		}
		notes = append(notes, "GitRepository did not handle the reconcile request in "+(timeout/2).String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if revision != "" {
		return "main@sha1:" + revision, notes, nil
	}
	if rev, ok := f.Revisions[site]; ok {
		return rev, notes, nil
	}
	return f.Revision, notes, nil
}

// VerifyGit implements engine.Flux.
func (f *Flux) VerifyGit(_ context.Context, site string, held []failoverv1.HeldObject, _ []failoverv1.ObjectField, _ string, _ bool) (string, error) {
	return "Git equals live", f.call("VerifyGit", site)
}

// VerifyNoop implements engine.Flux. A held object whose generation is not
// the one recorded at release stops the run with FluxChanged when strict.
func (f *Flux) VerifyNoop(_ context.Context, site string, held []failoverv1.HeldObject, _ []failoverv1.ObjectField, strict bool, _ time.Duration) (string, error) {
	if err := f.call("VerifyNoop", site); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range held {
		if g := f.generation(key(site, h.Object)); h.Generation != 0 && g != h.Generation && strict {
			return "", stop.Ask(failoverv1.ReasonFluxChanged, "%s generation %d at release, now %d", h.Object.Name, h.Generation, g)
		}
	}
	return "nothing changed", nil
}

// Git is a fake engine.Git. It maps each field as spec.git.fields says,
// else to clusters/<site>/<kind>.yaml when spec.git.find is set; a field
// with neither has no mapping.
// The branch's history is the commits Commit made, in order; a Head set by
// a test on top of them is a commit pushed after them.
type Git struct {
	mu          sync.Mutex
	Unreachable bool
	Head        string
	Commits     []engine.CommitRequest
	// Checkouts are the revisions Checkout was asked for.
	Checkouts []string
	made      []string
}

// Resolve implements engine.Git.
func (g *Git) Resolve(_ context.Context, w *failoverv1.WorkloadSpec, changes []driver.FieldChange) ([]failoverv1.GitChange, error) {
	var out []failoverv1.GitChange
	for _, c := range changes {
		gc := failoverv1.GitChange{Site: c.Site, Object: c.Object, Field: c.Field, Value: c.Value}
		var m *failoverv1.FieldMapping
		for i, f := range w.Git.Fields {
			if f.Site == c.Site && f.Object.Kind == c.Object.Kind && f.Object.Name == c.Object.Name && f.Field == c.Field {
				m = &w.Git.Fields[i]
			}
		}
		switch {
		case m == nil && w.Git.Find == nil:
			return nil, fmt.Errorf("%w: site %s, %s %s, field %s", engine.ErrNoMapping, c.Site, c.Object.Kind, c.Object.Name, c.Field)
		case g.Unreachable:
		case m != nil:
			gc.File, gc.Path = m.File, m.Path
		default:
			gc.File = fmt.Sprintf("clusters/%s/%s.yaml", c.Site, lower(c.Object.Kind))
			gc.Path = c.Field
		}
		out = append(out, gc)
	}
	if g.Unreachable {
		return out, fmt.Errorf("git: remote unreachable")
	}
	return out, nil
}

// HasMapping implements engine.Git, as Resolve maps a change.
func (g *Git) HasMapping(w *failoverv1.WorkloadSpec, c driver.FieldChange) bool {
	for _, f := range w.Git.Fields {
		if f.Site == c.Site && f.Object.Kind == c.Object.Kind && f.Object.Name == c.Object.Name && f.Field == c.Field {
			return true
		}
	}
	return w.Git.Find != nil
}

// Commit implements engine.Git.
func (g *Git) Commit(_ context.Context, _ *failoverv1.WorkloadSpec, req engine.CommitRequest) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Unreachable {
		return "", stop.Wait(failoverv1.ReasonGitUnreachable, "remote unreachable")
	}
	if g.Head != "" && g.Head != req.Base {
		return "", stop.AskHead(failoverv1.ReasonGitChanged, g.Head, "branch moved from %s to %s", req.Base, g.Head)
	}
	g.Commits = append(g.Commits, req)
	g.Head = fmt.Sprintf("c%039d", len(g.Commits))
	g.made = append(g.made, g.Head)
	return g.Head, nil
}

// Checkout implements engine.Git.
func (g *Git) Checkout(_ context.Context, _ *failoverv1.WorkloadSpec, rev string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Checkouts = append(g.Checkouts, rev)
	return "/nonexistent", nil
}

// BranchHead implements engine.Git.
func (g *Git) BranchHead(context.Context, *failoverv1.WorkloadSpec) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Unreachable {
		return "", stop.Wait(failoverv1.ReasonGitUnreachable, "remote unreachable")
	}
	return g.Head, nil
}

// Contains implements engine.Git. Head, when Commit did not make it, comes
// after every commit Commit made.
func (g *Git) Contains(_ context.Context, _ *failoverv1.WorkloadSpec, commit, rev string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if commit == rev {
		return true, nil
	}
	order := g.made
	if g.Head != "" && !slices.Contains(order, g.Head) {
		order = append(slices.Clone(order), g.Head)
	}
	i, j := slices.Index(order, commit), slices.Index(order, rev)
	return i >= 0 && j >= 0 && i <= j, nil
}

// Close implements engine.Git.
func (g *Git) Close() error { return nil }

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// withPods adds, for each CloudNativePG Cluster among objs, the Pod that
// its status names current primary, as CloudNativePG v1.30.1 runs it: its
// postgres container serves the status port with TLS.
func withPods(objs []client.Object) []client.Object {
	out := slices.Clone(objs)
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok || u.GetKind() != "Cluster" {
			continue
		}
		name, _, _ := unstructured.NestedString(u.Object, "status", "currentPrimary")
		if name == "" {
			continue
		}
		has := false
		for _, p := range objs {
			has = has || p.GetName() == name && p.GetNamespace() == u.GetNamespace() && p.GetObjectKind().GroupVersionKind().Kind == "Pod"
		}
		if !has {
			out = append(out, Pod(u.GetNamespace(), name))
		}
	}
	return out
}

// Pod returns an instance Pod of CloudNativePG.
func Pod(namespace, name string) *corev1.Pod {
	p := &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}}
	p.Namespace, p.Name = namespace, name
	p.Spec.Containers = []corev1.Container{{Name: "postgres", Command: []string{"/controller/manager", "instance", "run", "--status-port-tls"}}}
	return p
}

// Proxied is one GET that the fake API server proxied to a pod.
type Proxied struct {
	Site, Namespace, Pod, Scheme, Path string
	Port                               int
}

// SetStatus sets the instance status that the site's pods report, as
// JSON; nil goes back to the default. An error makes the read fail.
func (c *Clients) SetStatus(site string, status map[string]any, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == nil {
		c.status = map[string]map[string]any{}
		c.statusErr = map[string]error{}
	}
	c.status[site], c.statusErr[site] = status, err
}

// ProxyLog returns the GETs proxied so far.
func (c *Clients) ProxyLog() []Proxied {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.proxied)
}

// ProxyGet implements kube.Proxier. By default a pod reports the status
// of an instance at 0/3000060 on timeline 1: a primary when its Cluster
// reports it so, else a replica that streams.
func (c *Clients) ProxyGet(ctx context.Context, site, namespace, pod, scheme string, port int, path string) ([]byte, error) {
	if c.IsDown(site) {
		return nil, &kube.UnreachableError{Err: fmt.Errorf("site %s is unreachable: connection refused", site)}
	}
	c.mu.Lock()
	c.proxied = append(c.proxied, Proxied{Site: site, Namespace: namespace, Pod: pod, Scheme: scheme, Path: path, Port: port})
	status, err := c.status[site], c.statusErr[site]
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if status == nil {
		cl, err := c.Client(site)
		if err != nil {
			return nil, err
		}
		u := Object("postgresql.cnpg.io/v1", "Cluster", namespace, "pg-"+site, nil)
		if err := cl.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
			return nil, err
		}
		primary, _, _ := unstructured.NestedBool(u.Object, "status", "instancesReportedState", pod, "isPrimary")
		tl, _, _ := unstructured.NestedInt64(u.Object, "status", "timelineID")
		if tl == 0 {
			tl = 1
		}
		status = map[string]any{"systemID": "7300000000000000001", "timeLineID": tl, "isPrimary": primary}
		if primary {
			status["currentLsn"] = "0/3000060"
			status["currentWAL"] = fmt.Sprintf("%08X0000000000000003", tl)
		}
		// An instance whose Cluster archives WAL has archived the history
		// file of its timeline, unless FaultArchive.
		_, store, _ := unstructured.NestedMap(u.Object, "spec", "backup", "barmanObjectStore")
		if store && c.Fault(FaultArchive) == "" {
			status["lastArchivedWAL"], status["isArchivingWAL"] = fmt.Sprintf("%08X.history", tl), true
		} else {
			status["receivedLsn"], status["replayLsn"], status["isWalReceiverActive"] = "0/3000060", "0/3000060", true
		}
	}
	return json.Marshal(status)
}
