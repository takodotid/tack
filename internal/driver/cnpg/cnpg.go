// Package cnpg is the driver for CloudNativePG in its distributed topology:
// one Cluster per site, the primary chosen by .spec.replica.primary, and
// a demotion token that hands the primary over without losing writes.
package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/version"
)

// Name is the driver's name in the Workload.
const Name = "cnpg"

// APIVersion of CloudNativePG's Cluster and Pooler.
const APIVersion = "postgresql.cnpg.io/v1"

// PhaseHealthy is the Cluster phase of a healthy cluster.
const PhaseHealthy = "Cluster in healthy state"

// TokenKey names the demotion token in the ledger.
const TokenKey = "demotionToken"

// Field paths the driver changes.
const (
	FieldPrimary   = "spec.replica.primary"
	FieldToken     = "spec.replica.promotionToken"
	FieldInstances = "spec.instances"
)

// Facts the driver reads.
const (
	factCluster       = "cluster"
	factSelf          = "self"
	factPrimary       = "primary"
	factEnabled       = "enabled"
	factPhase         = "phase"
	factInProgress    = "inProgress"
	factMinApplyDelay = "minApplyDelay"
	factIsPrimary     = "isPrimary"
	// factExternal names the Cluster's spec.externalClusters, comma-separated.
	factExternal = "externalClusters"
)

func init() { driver.Register(Driver{}) }

// Driver implements driver.Driver for CloudNativePG.
type Driver struct{}

// Name implements driver.Driver.
func (Driver) Name() string { return Name }

// Validate implements driver.Driver.
func (Driver) Validate(w *failoverv1.WorkloadSpec) error {
	var errs []string
	names := map[string]string{}
	for _, s := range w.Sites {
		if s.CNPG == nil || s.CNPG.Cluster.Name == "" || s.CNPG.Cluster.Namespace == "" {
			errs = append(errs, fmt.Sprintf("site %s: spec.sites[].cnpg.cluster needs namespace and name", s.Name))
			continue
		}
		if other, dup := names[s.CNPG.Cluster.Name]; dup {
			errs = append(errs, fmt.Sprintf("sites %s and %s name the same Cluster %q: each site's Cluster needs its own name", other, s.Name, s.CNPG.Cluster.Name))
		}
		names[s.CNPG.Cluster.Name] = s.Name
		if p := s.CNPG.Pooler; p != nil && (p.Name == "" || p.Namespace == "") {
			errs = append(errs, fmt.Sprintf("site %s: spec.sites[].cnpg.pooler needs namespace and name", s.Name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cnpg: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ClusterRef is the site's Cluster.
func ClusterRef(s *failoverv1.Site) failoverv1.ObjectRef {
	return failoverv1.ObjectRef{APIVersion: APIVersion, Kind: "Cluster", Namespace: s.CNPG.Cluster.Namespace, Name: s.CNPG.Cluster.Name}
}

// PoolerRef is the site's read-write Pooler.
func PoolerRef(s *failoverv1.Site) failoverv1.ObjectRef {
	return failoverv1.ObjectRef{APIVersion: APIVersion, Kind: "Pooler", Namespace: s.CNPG.Pooler.Namespace, Name: s.CNPG.Pooler.Name}
}

// deploymentRef is the Deployment CloudNativePG runs for a Pooler.
func deploymentRef(s *failoverv1.Site) failoverv1.ObjectRef {
	return failoverv1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: s.CNPG.Pooler.Namespace, Name: s.CNPG.Pooler.Name}
}

func objects(s *failoverv1.Site) []failoverv1.ObjectRef {
	out := []failoverv1.ObjectRef{ClusterRef(s)}
	if s.CNPG.Pooler != nil {
		out = append(out, PoolerRef(s))
	}
	return out
}

// Objects implements driver.Driver: the site's Cluster, and its Pooler
// when the Workload names one.
func (Driver) Objects(w *failoverv1.WorkloadSpec, site string) []failoverv1.ObjectRef {
	s := siteByName(w, site)
	if s == nil || s.CNPG == nil {
		return nil
	}
	return objects(s)
}

// Discover implements driver.Driver.
func (Driver) Discover(ctx context.Context, w *failoverv1.WorkloadSpec, c kube.Clients) (*driver.Topology, error) {
	byCluster := map[string]string{}
	for _, s := range w.Sites {
		byCluster[s.CNPG.Cluster.Name] = s.Name
	}
	t := &driver.Topology{}
	for i := range w.Sites {
		s := &w.Sites[i]
		st := driver.SiteState{Name: s.Name, Role: driver.RoleUnknown, Objects: objects(s), Facts: map[string]string{}}
		if err := c.Reachable(ctx, s.Name); err != nil {
			if !kube.IsUnreachable(err) {
				return nil, fmt.Errorf("site %s: %w", s.Name, err)
			}
			st.Error = err.Error()
			t.Sites = append(t.Sites, st)
			continue
		}
		st.Reachable = true
		cl, err := c.Client(s.Name)
		if err != nil {
			return nil, err
		}
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(APIVersion)
		u.SetKind("Cluster")
		if err := cl.Get(ctx, client.ObjectKey{Namespace: s.CNPG.Cluster.Namespace, Name: s.CNPG.Cluster.Name}, u); err != nil {
			return nil, fmt.Errorf("site %s: read Cluster %s/%s: %w", s.Name, s.CNPG.Cluster.Namespace, s.CNPG.Cluster.Name, err)
		}
		read(&st, u, byCluster)
		st.Created = u.GetCreationTimestamp().Time
		st.Position = position(ctx, c, s.Name, u)
		if s.CNPG.Pooler != nil {
			p := &unstructured.Unstructured{}
			p.SetAPIVersion(APIVersion)
			p.SetKind("Pooler")
			if err := cl.Get(ctx, client.ObjectKey{Namespace: s.CNPG.Pooler.Namespace, Name: s.CNPG.Pooler.Name}, p); err != nil {
				return nil, fmt.Errorf("site %s: read Pooler %s/%s: %w", s.Name, s.CNPG.Pooler.Namespace, s.CNPG.Pooler.Name, err)
			}
			n, _, _ := unstructured.NestedInt64(p.Object, "spec", "instances")
			st.Summary += fmt.Sprintf("; Pooler %s has %d instances", s.CNPG.Pooler.Name, n)
		}
		t.Sites = append(t.Sites, st)
	}
	stand(t)
	return t, nil
}

// read fills a site's state from its Cluster.
func read(st *driver.SiteState, u *unstructured.Unstructured, byCluster map[string]string) {
	f := st.Facts
	f[factCluster] = u.GetName()
	self, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "self")
	if self == "" {
		self = u.GetName()
	}
	primary, hasPrimary, _ := unstructured.NestedString(u.Object, "spec", "replica", "primary")
	enabled, hasEnabled, _ := unstructured.NestedBool(u.Object, "spec", "replica", "enabled")
	_, hasReplica, _ := unstructured.NestedMap(u.Object, "spec", "replica")
	f[factSelf], f[factPrimary] = self, primary
	if hasEnabled {
		f[factEnabled] = fmt.Sprint(enabled)
	}
	delay, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "minApplyDelay")
	f[factMinApplyDelay] = delay
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	f[factPhase] = phase
	external, _, _ := unstructured.NestedSlice(u.Object, "spec", "externalClusters")
	var names []string
	for _, e := range external {
		if m, ok := e.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	f[factExternal] = strings.Join(names, ",")
	inProgress, _, _ := unstructured.NestedBool(u.Object, "status", "switchReplicaClusterStatus", "inProgress")
	f[factInProgress] = fmt.Sprint(inProgress)
	isPrimary := false
	states, _, _ := unstructured.NestedMap(u.Object, "status", "instancesReportedState")
	for _, v := range states {
		if m, ok := v.(map[string]any); ok && m["isPrimary"] == true {
			isPrimary = true
		}
	}
	f[factIsPrimary] = fmt.Sprint(isPrimary)
	current, _, _ := unstructured.NestedString(u.Object, "status", "currentPrimary")

	replica := hasReplica && (hasEnabled && enabled || !hasEnabled && hasPrimary && self != primary)
	switch {
	case hasEnabled:
		st.Role = driver.RoleUnknown
	case replica:
		st.Role = driver.RoleReplica
	case isPrimary:
		st.Role = driver.RolePrimary
	}
	st.DeclaredPrimary = byCluster[primary]
	if primary != "" && st.DeclaredPrimary == "" {
		st.DeclaredPrimary = "unknown Cluster " + primary
	}
	st.Healthy = phase == PhaseHealthy && !inProgress
	st.Summary = fmt.Sprintf("Cluster %s/%s: phase %q, declares %s primary, pod %s",
		u.GetNamespace(), u.GetName(), phase, orNone(primary), orNone(current))
	if inProgress {
		st.Summary += ", demotion in progress"
	}
	if hasEnabled {
		st.Summary += ", uses spec.replica.enabled (Tack needs spec.replica.primary)"
	}
}

// Plan implements driver.Driver: the refusals of the topology, then Steps.
func (d Driver) Plan(w *failoverv1.WorkloadSpec, t *driver.Topology, r driver.Request) ([]failoverv1.Step, []failoverv1.FollowUp, error) {
	if _, _, err := sites(w, r); err != nil {
		return nil, nil, err
	}
	if err := check(t, r); err != nil {
		return nil, nil, err
	}
	steps, follow, err := d.Steps(w, r)
	if err != nil {
		return nil, nil, err
	}
	// A site that holds WAL past to cannot follow it ([RFC#0004]
	// Requirement 14): check refuses it unless the operator accepted it.
	if r.Mode == failoverv1.ModeSiteLoss {
		toSt, _ := t.Site(r.To)
		for _, s := range ahead(t, r) {
			site := siteByName(w, s.Name)
			follow = append(follow, failoverv1.FollowUp{
				Site:   s.Name,
				Action: engine.FollowUpRebuild,
				Reason: fmt.Sprintf("%s when %s was promoted, so Cluster %s may hold writes that %s lacks and cannot follow it: recreate %s from %s (%s)",
					aheadOf(&s, toSt), to(w, r), site.CNPG.Cluster.Name, to(w, r), site.CNPG.Cluster.Name, to(w, r), version.DocLink("docs/runbooks/rebuild-old-primary.md")),
			})
		}
	}
	return steps, follow, nil
}

// to is the name of the Cluster that becomes primary.
func to(w *failoverv1.WorkloadSpec, r driver.Request) string {
	return siteByName(w, r.To).CNPG.Cluster.Name
}

// check refuses a request that the topology does not allow.
func check(t *driver.Topology, r driver.Request) error {
	fromSt, _ := t.Site(r.From)
	toSt, _ := t.Site(r.To)
	if err := checkTarget(toSt, r); err != nil {
		return err
	}
	// Each other site's Cluster will declare to's primary: CloudNativePG
	// refuses a primary that it does not name among its external clusters.
	// A site loss goes on: its notes say where the run stops (Notes).
	if l := lacksTo(t, r); r.Mode == failoverv1.ModePlanned && len(l) > 0 {
		s := l[0]
		return fmt.Errorf("site %s: Cluster %s does not name %s among its spec.externalClusters, so CloudNativePG would refuse to make it declare %s primary; add %s there, through Git, before the run",
			s.Name, s.Facts[factCluster], toSt.Facts[factCluster], toSt.Facts[factCluster], toSt.Facts[factCluster])
	}
	for _, s := range t.Sites {
		if s.Reachable && s.Facts[factEnabled] != "" {
			return fmt.Errorf("site %s: Cluster %s uses spec.replica.enabled; Tack needs the distributed topology with spec.replica.primary", s.Name, s.Facts[factCluster])
		}
		if s.Reachable && s.Facts[factInProgress] == "true" {
			return fmt.Errorf("site %s: a demotion is in progress; wait for it to finish", s.Name)
		}
	}
	switch r.Mode {
	case failoverv1.ModePlanned:
		if fromSt == nil || !fromSt.Reachable {
			return fmt.Errorf("site %s is unreachable: a planned switchover needs it", r.From)
		}
		if fromSt.Role != driver.RolePrimary || !fromSt.Healthy {
			return fmt.Errorf("site %s is not a healthy primary (%s): a planned switchover needs one", r.From, fromSt.Summary)
		}
		for _, s := range t.Sites {
			if s.Reachable && s.DeclaredPrimary != r.From {
				return fmt.Errorf("site %s declares %s primary, not %s: the declarations disagree; fix them before a switchover", s.Name, orNone(s.DeclaredPrimary), r.From)
			}
		}
		if toSt.Facts[factMinApplyDelay] != "" {
			return fmt.Errorf("site %s: Cluster sets spec.replica.minApplyDelay, which CloudNativePG does not allow with a promotion token", r.To)
		}
	case failoverv1.ModeSiteLoss:
		if fromSt != nil && fromSt.Reachable && fromSt.Role == driver.RolePrimary {
			return fmt.Errorf("site %s answers as primary (%s): use a planned switchover, or stop it first", r.From, fromSt.Summary)
		}
		if a := ahead(t, r); len(a) > 0 && !r.AcceptBehind {
			s := a[0]
			return fmt.Errorf("site %s holds WAL past site %s: %s. Promoting %s loses the writes that %s holds, and site %s must then be rebuilt. Promote the most caught-up site instead (--to %s), or pass --accept-behind to promote %s anyway",
				s.Name, r.To, aheadOf(&s, toSt), r.To, s.Name, s.Name, best(t, a), r.To)
		}
	default:
		return fmt.Errorf("unknown mode %q", r.Mode)
	}
	return nil
}

// lacksTo returns the sites that answer, other than to, whose Cluster does
// not name to's among its external clusters ([RFC#0004] Requirement 12).
func lacksTo(t *driver.Topology, r driver.Request) []driver.SiteState {
	toSt, ok := t.Site(r.To)
	if !ok {
		return nil
	}
	var out []driver.SiteState
	for _, s := range t.Sites {
		if s.Reachable && s.Name != r.To && !slices.Contains(strings.Split(s.Facts[factExternal], ","), toSt.Facts[factCluster]) {
			out = append(out, s)
		}
	}
	return out
}

// Notes implements driver.Noter. A site loss whose sites lack to's Cluster
// among their external clusters is not refused: Git, the way to add it,
// may have gone with the lost site. The step that makes such a site
// declare to's primary fails, as CloudNativePG refuses it, and the run
// stops there.
func (d Driver) Notes(w *failoverv1.WorkloadSpec, t *driver.Topology, r driver.Request) []string {
	notes := positionNotes(w, t, r)
	if r.Mode != failoverv1.ModeSiteLoss {
		return notes
	}
	toSt, ok := t.Site(r.To)
	if !ok {
		return notes
	}
	to := toSt.Facts[factCluster]
	for _, s := range lacksTo(t, r) {
		when := "after the promotion of " + to
		if s.Name == r.From {
			when = "before the promotion of " + to
		}
		notes = append(notes, fmt.Sprintf("warning: site %s: Cluster %s does not name %s among its spec.externalClusters, so CloudNativePG refuses to make it declare %s primary: the run stops at step %s, %s, and that step changes nothing. To go on, add %s to that list, at the site and in Git, then resume the run",
			s.Name, s.Facts[factCluster], to, to, d.Demotion(r, s.Name), when, to))
	}
	return notes
}

// Steps implements driver.Driver: the steps depend on the Workload and the
// request only, so a resume can compose them again from the ledger.
func (d Driver) Steps(w *failoverv1.WorkloadSpec, r driver.Request) ([]failoverv1.Step, []failoverv1.FollowUp, error) {
	from, to, err := sites(w, r)
	if err != nil {
		return nil, nil, err
	}
	toName := to.CNPG.Cluster.Name
	b := &builder{w: w, toName: toName}
	var follow []failoverv1.FollowUp

	switch r.Mode {
	case failoverv1.ModePlanned:
		if from.CNPG.Pooler != nil {
			b.scale(from, 0)
		}
		b.patch("demote-"+from.Name, from, ClusterRef(from), "demote Cluster "+from.CNPG.Cluster.Name+": declare "+toName+" primary and drop any old token",
			replicaPatch(toName, nil))
		b.wait("wait-demoted-"+from.Name, from, ClusterRef(from), "wait until Cluster "+from.CNPG.Cluster.Name+" has demoted and published its demotion token",
			w.Timeouts.Demotion.Duration,
			cond("{.status.demotionToken}", failoverv1.OpNotEmpty, ""),
			cond("{.status.switchReplicaClusterStatus.inProgress}", failoverv1.OpNotEquals, "true"))
		b.add(failoverv1.Step{
			ID: "capture-token", Type: failoverv1.StepCapture, Site: from.Name,
			Description: "record the demotion token of Cluster " + from.CNPG.Cluster.Name + " in the ledger",
			Capture:     &failoverv1.CaptureStep{Object: ClusterRef(from), JSONPath: "{.status.demotionToken}", Into: TokenKey},
		})
		token := "$(" + TokenKey + ")"
		b.patch("promote-"+to.Name, to, ClusterRef(to), "promote Cluster "+toName+" with the demotion token, in one patch",
			replicaPatch(toName, &token))
		b.wait("wait-promoted-"+to.Name, to, ClusterRef(to), "wait until Cluster "+toName+" has used the token and runs a healthy primary",
			w.Timeouts.Promotion.Duration,
			cond("{.status.lastPromotionToken}", failoverv1.OpEquals, token),
			cond("{.status.instancesReportedState.*.isPrimary}", failoverv1.OpContains, "true"),
			cond("{.status.phase}", failoverv1.OpEquals, PhaseHealthy))
		b.patch("clear-token-"+to.Name, to, ClusterRef(to), "remove the used token from Cluster "+toName+", so it can never apply again",
			`{"spec":{"replica":{"promotionToken":null}}}`)
		b.declareOthers(r)
		b.poolers(r)

	case failoverv1.ModeSiteLoss:
		b.patch("fence-"+from.Name, from, ClusterRef(from), "fence Cluster "+from.CNPG.Cluster.Name+": declare "+toName+" primary, so it never runs as primary again",
			replicaPatch(toName, nil))
		if from.CNPG.Pooler != nil {
			b.scale(from, 0)
		}
		if !r.Legacy {
			b.record("record-position-"+to.Name, to, ClusterRef(to), RecordPosition, nil,
				"record in the ledger how far Cluster "+toName+" reaches right before its promotion: the old primary's writes after it are not in the new history")
		}
		b.patch("promote-"+to.Name, to, ClusterRef(to), "promote Cluster "+toName+" without a token",
			replicaPatch(toName, nil))
		b.wait("wait-promoted-"+to.Name, to, ClusterRef(to), "wait until Cluster "+toName+" runs a healthy primary",
			w.Timeouts.Promotion.Duration,
			cond("{.status.instancesReportedState.*.isPrimary}", failoverv1.OpContains, "true"),
			cond("{.status.phase}", failoverv1.OpEquals, PhaseHealthy))
		if !r.Legacy {
			b.record("record-fork-"+to.Name, to, ClusterRef(to), RecordFork, nil,
				"record in the ledger the timeline on which Cluster "+toName+"'s new history starts")
		}
		b.declareOthers(r)
		if to.CNPG.Pooler != nil {
			b.scale(to, to.CNPG.Pooler.Instances)
		}
		if !r.Legacy {
			b.keepOld(from)
		}
		follow = append(follow, failoverv1.FollowUp{
			Site:   from.Name,
			Action: engine.FollowUpRebuild,
			Reason: fmt.Sprintf("Cluster %s was promoted without a token, so Cluster %s may hold writes that %s lacks and cannot follow it: keep them for their owners (%s), then recreate %s from %s (%s)",
				toName, from.CNPG.Cluster.Name, toName, version.DocLink("docs/runbooks/recover-old-primary-writes.md"), from.CNPG.Cluster.Name, toName, version.DocLink("docs/runbooks/rebuild-old-primary.md")),
		})
	default:
		return nil, nil, fmt.Errorf("unknown mode %q", r.Mode)
	}
	verify, err := d.Verify(w, r, r.To)
	if err != nil {
		return nil, nil, err
	}
	b.steps = append(b.steps, verify...)
	return b.steps, follow, nil
}

// Demotion implements driver.Driver: demote-<from> in a planned run,
// fence-<from> in a site loss, declare-<site> at a third site. The IDs are
// unchanged since v1.0.0.
func (Driver) Demotion(r driver.Request, site string) string {
	switch {
	case site == r.To:
		return ""
	case site != r.From:
		return "declare-" + site
	case r.Mode == failoverv1.ModeSiteLoss:
		return "fence-" + site
	}
	return "demote-" + site
}

func checkTarget(toSt *driver.SiteState, r driver.Request) error {
	if toSt == nil || !toSt.Reachable {
		return fmt.Errorf("site %s must be reachable to become primary", r.To)
	}
	if toSt.Role != driver.RoleReplica {
		return fmt.Errorf("site %s is not a replica (%s)", r.To, toSt.Summary)
	}
	if !toSt.Healthy {
		return fmt.Errorf("site %s is not healthy (%s)", r.To, toSt.Summary)
	}
	return nil
}

// GitChange implements driver.Driver. Git never holds a promotion token:
// a used token that comes back after removal makes CloudNativePG refuse to
// run the cluster.
func (Driver) GitChange(w *failoverv1.WorkloadSpec, _ *driver.Topology, r driver.Request) ([]driver.FieldChange, error) {
	_, to, err := sites(w, r)
	if err != nil {
		return nil, err
	}
	primary := jsonString(to.CNPG.Cluster.Name)
	var out []driver.FieldChange
	for i := range w.Sites {
		s := &w.Sites[i]
		out = append(out,
			driver.FieldChange{Site: s.Name, Object: ClusterRef(s), Field: FieldPrimary, Value: &primary},
			driver.FieldChange{Site: s.Name, Object: ClusterRef(s), Field: FieldToken, Optional: true},
		)
		if s.CNPG.Pooler != nil {
			n := "0"
			if s.Name == r.To {
				n = fmt.Sprint(s.CNPG.Pooler.Instances)
			}
			out = append(out, driver.FieldChange{Site: s.Name, Object: PoolerRef(s), Field: FieldInstances, Value: &n})
		}
	}
	return out, nil
}

// Verify implements driver.Driver: the new primary's Cluster runs a healthy
// primary; any other site's Cluster declares it.
func (Driver) Verify(w *failoverv1.WorkloadSpec, r driver.Request, site string) ([]failoverv1.Step, error) {
	_, to, err := sites(w, r)
	if err != nil {
		return nil, err
	}
	s := siteByName(w, site)
	if s == nil {
		return nil, fmt.Errorf("site %q is not in the workload", site)
	}
	b := &builder{w: w}
	if site == r.To {
		b.wait("verify-"+site, s, ClusterRef(s), "verify that Cluster "+s.CNPG.Cluster.Name+" is a healthy primary and declares itself",
			w.Timeouts.Promotion.Duration,
			cond("{.spec.replica.primary}", failoverv1.OpEquals, to.CNPG.Cluster.Name),
			cond("{.status.instancesReportedState.*.isPrimary}", failoverv1.OpContains, "true"),
			cond("{.status.phase}", failoverv1.OpEquals, PhaseHealthy))
	} else {
		b.wait("verify-"+site, s, ClusterRef(s), "verify that Cluster "+s.CNPG.Cluster.Name+" declares "+to.CNPG.Cluster.Name+" primary",
			w.Timeouts.Promotion.Duration,
			cond("{.spec.replica.primary}", failoverv1.OpEquals, to.CNPG.Cluster.Name))
	}
	return b.steps, nil
}

type builder struct {
	w      *failoverv1.WorkloadSpec
	toName string
	steps  []failoverv1.Step
}

func (b *builder) add(s failoverv1.Step) {
	s.Phase = failoverv1.PhaseFailover
	b.steps = append(b.steps, s)
}

// record adds a Record step.
func (b *builder) record(id string, s *failoverv1.Site, obj failoverv1.ObjectRef, what string, timeout *time.Duration, desc string) {
	rs := &failoverv1.RecordStep{Object: obj, What: what}
	if timeout != nil {
		rs.Timeout = &metav1.Duration{Duration: *timeout}
	}
	b.add(failoverv1.Step{ID: id, Type: failoverv1.StepRecord, Site: s.Name, Description: desc, Record: rs})
}

// keepOld keeps the old primary's data for the operator, once its site
// answers: a snapshot of its volumes, when the Workload asks for one, and
// its WAL archive, both recorded on its follow-up Rebuild ([RFC#0004]
// Requirement 15). The snapshot is cold: CloudNativePG shuts the instance
// down for it, so it needs no WAL archive to start from, and recommends a
// cold snapshot for a recovery (v1.30.1,
// docs/src/appendixes/backup_volumesnapshot.md:130-150). The instance no
// longer takes writes: it was fenced first.
func (b *builder) keepOld(from *failoverv1.Site) {
	cluster := from.CNPG.Cluster
	if !from.CNPG.Snapshot {
		b.record("record-archive-"+from.Name, from, ClusterRef(from), RecordArchive, nil,
			"record on the follow-up Rebuild where Cluster "+cluster.Name+" archives its WAL, so the rebuild keeps it")
		return
	}
	ref := failoverv1.ObjectRef{APIVersion: APIVersion, Kind: "Backup", Namespace: cluster.Namespace, Name: "$(run)-" + from.Name}
	manifest := map[string]any{
		"apiVersion": APIVersion, "kind": "Backup",
		"metadata": map[string]any{"namespace": cluster.Namespace, "name": ref.Name},
		"spec":     map[string]any{"cluster": map[string]any{"name": cluster.Name}, "method": "volumeSnapshot", "target": "primary", "online": false},
	}
	body, _ := json.Marshal(manifest)
	b.add(failoverv1.Step{ID: "snapshot-" + from.Name, Type: failoverv1.StepCreate, Site: from.Name,
		Description: "take a snapshot of the volumes of Cluster " + cluster.Name + " with a CloudNativePG Backup, before anyone rebuilds it",
		Create:      &failoverv1.CreateStep{Object: ref, Manifest: string(body)}})
	timeout := DefaultSnapshotTimeout
	if t := b.w.Timeouts.Snapshot; t != nil {
		timeout = t.Duration
	}
	b.record("record-snapshot-"+from.Name, from, ref, RecordSnapshot, &timeout,
		"wait until the Backup completes, then record its VolumeSnapshots and the WAL archive of Cluster "+cluster.Name+" on the follow-up Rebuild")
}

func (b *builder) patch(id string, s *failoverv1.Site, obj failoverv1.ObjectRef, desc, merge string) {
	b.add(failoverv1.Step{ID: id, Type: failoverv1.StepPatch, Site: s.Name, Description: desc,
		Patch: &failoverv1.PatchStep{Object: obj, Merge: merge}})
}

func (b *builder) wait(id string, s *failoverv1.Site, obj failoverv1.ObjectRef, desc string, timeout time.Duration, conds ...failoverv1.Condition) {
	b.add(failoverv1.Step{ID: id, Type: failoverv1.StepWait, Site: s.Name, Description: desc,
		Wait: &failoverv1.WaitStep{Object: obj, Conditions: conds, Timeout: metav1.Duration{Duration: timeout}}})
}

// declareOthers makes every other site's Cluster declare the new primary.
func (b *builder) declareOthers(r driver.Request) {
	for i := range b.w.Sites {
		s := &b.w.Sites[i]
		if s.Name == r.From || s.Name == r.To {
			continue
		}
		b.patch("declare-"+s.Name, s, ClusterRef(s), "make Cluster "+s.CNPG.Cluster.Name+" declare "+b.toName+" primary",
			replicaPatch(b.toName, nil))
	}
}

// poolers runs the read-write Pooler at the new primary's site, and at
// no site other than from, whose Pooler went to 0 before the demotion.
func (b *builder) poolers(r driver.Request) {
	if to := siteByName(b.w, r.To); to.CNPG.Pooler != nil {
		b.scale(to, to.CNPG.Pooler.Instances)
	}
	for i := range b.w.Sites {
		s := &b.w.Sites[i]
		if s.Name != r.To && s.Name != r.From && s.CNPG.Pooler != nil {
			b.scale(s, 0)
		}
	}
}

func (b *builder) scale(s *failoverv1.Site, n int32) {
	b.patch(fmt.Sprintf("pooler-%s", s.Name), s, PoolerRef(s), fmt.Sprintf("set Pooler %s to %d instances", s.CNPG.Pooler.Name, n),
		fmt.Sprintf(`{"spec":{"instances":%d}}`, n))
	if n == 0 {
		b.wait("wait-pooler-"+s.Name, s, deploymentRef(s), fmt.Sprintf("wait until the Pooler %s runs no pod", s.CNPG.Pooler.Name),
			b.w.Timeouts.Promotion.Duration, cond("{.status.replicas}", failoverv1.OpEmpty, ""))
		return
	}
	b.wait("wait-pooler-"+s.Name, s, deploymentRef(s), fmt.Sprintf("wait until the Pooler %s has %d ready pods", s.CNPG.Pooler.Name, n),
		b.w.Timeouts.Promotion.Duration, cond("{.status.readyReplicas}", failoverv1.OpEquals, fmt.Sprint(n)))
}

func replicaPatch(primary string, token *string) string {
	tok := "null"
	if token != nil {
		tok = jsonString(*token)
	}
	return fmt.Sprintf(`{"spec":{"replica":{"primary":%s,"promotionToken":%s}}}`, jsonString(primary), tok)
}

func cond(path string, op failoverv1.Operator, value string) failoverv1.Condition {
	return failoverv1.Condition{JSONPath: path, Operator: op, Value: value}
}

func sites(w *failoverv1.WorkloadSpec, r driver.Request) (from, to *failoverv1.Site, err error) {
	from, to = siteByName(w, r.From), siteByName(w, r.To)
	if from == nil || to == nil {
		return nil, nil, fmt.Errorf("sites %q and %q must both be in the workload", r.From, r.To)
	}
	return from, to, nil
}

func siteByName(w *failoverv1.WorkloadSpec, name string) *failoverv1.Site {
	for i := range w.Sites {
		if w.Sites[i].Name == name {
			return &w.Sites[i]
		}
	}
	return nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
