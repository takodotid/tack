// Package cnpg is the driver for CloudNativePG in its distributed topology:
// one Cluster per site, the primary chosen by .spec.replica.primary, and
// a demotion token that hands the primary over without losing writes.
package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
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
	inProgress, _, _ := unstructured.NestedBool(u.Object, "status", "switchReplicaClusterStatus", "inProgress")
	f[factInProgress] = fmt.Sprint(inProgress)
	// The instances report their own timeline; status.timelineID is stale
	// on a demoted Cluster.
	isPrimary, timeline := false, int64(0)
	states, _, _ := unstructured.NestedMap(u.Object, "status", "instancesReportedState")
	for _, v := range states {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if m["isPrimary"] == true {
			isPrimary = true
		}
		if tl, ok := m["timeLineID"].(int64); ok && tl > timeline {
			timeline = tl
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
	st.Summary = fmt.Sprintf("Cluster %s/%s: phase %q, declares %s primary, pod %s, timeline %d",
		u.GetNamespace(), u.GetName(), phase, orNone(primary), orNone(current), timeline)
	if inProgress {
		st.Summary += ", demotion in progress"
	}
	if hasEnabled {
		st.Summary += ", uses spec.replica.enabled (Tack needs spec.replica.primary)"
	}
}

// Plan implements driver.Driver.
func (d Driver) Plan(w *failoverv1.WorkloadSpec, t *driver.Topology, r driver.Request) ([]failoverv1.Step, []failoverv1.FollowUp, error) {
	from, to, err := sites(w, r)
	if err != nil {
		return nil, nil, err
	}
	fromSt, _ := t.Site(r.From)
	toSt, _ := t.Site(r.To)
	if err := checkTarget(toSt, r); err != nil {
		return nil, nil, err
	}
	for _, s := range t.Sites {
		if s.Reachable && s.Facts[factEnabled] != "" {
			return nil, nil, fmt.Errorf("site %s: Cluster %s uses spec.replica.enabled; Tack needs the distributed topology with spec.replica.primary", s.Name, s.Facts[factCluster])
		}
		if s.Reachable && s.Facts[factInProgress] == "true" {
			return nil, nil, fmt.Errorf("site %s: a demotion is in progress; wait for it to finish", s.Name)
		}
	}
	toName := to.CNPG.Cluster.Name
	b := &builder{w: w, toName: toName}
	var follow []failoverv1.FollowUp

	switch r.Mode {
	case failoverv1.ModePlanned:
		if fromSt == nil || !fromSt.Reachable {
			return nil, nil, fmt.Errorf("site %s is unreachable: a planned switchover needs it", r.From)
		}
		if fromSt.Role != driver.RolePrimary || !fromSt.Healthy {
			return nil, nil, fmt.Errorf("site %s is not a healthy primary (%s): a planned switchover needs one", r.From, fromSt.Summary)
		}
		for _, s := range t.Sites {
			if s.Reachable && s.DeclaredPrimary != r.From {
				return nil, nil, fmt.Errorf("site %s declares %s primary, not %s: the declarations disagree; fix them before a switchover", s.Name, orNone(s.DeclaredPrimary), r.From)
			}
		}
		if toSt.Facts[factMinApplyDelay] != "" {
			return nil, nil, fmt.Errorf("site %s: Cluster sets spec.replica.minApplyDelay, which CloudNativePG does not allow with a promotion token", r.To)
		}
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
		if fromSt != nil && fromSt.Reachable && fromSt.Role == driver.RolePrimary {
			return nil, nil, fmt.Errorf("site %s answers as primary (%s): use a planned switchover, or stop it first", r.From, fromSt.Summary)
		}
		b.patch("fence-"+from.Name, from, ClusterRef(from), "fence Cluster "+from.CNPG.Cluster.Name+": declare "+toName+" primary, so it never runs as primary again",
			replicaPatch(toName, nil))
		if from.CNPG.Pooler != nil {
			b.scale(from, 0)
		}
		b.patch("promote-"+to.Name, to, ClusterRef(to), "promote Cluster "+toName+" without a token",
			replicaPatch(toName, nil))
		b.wait("wait-promoted-"+to.Name, to, ClusterRef(to), "wait until Cluster "+toName+" runs a healthy primary",
			w.Timeouts.Promotion.Duration,
			cond("{.status.instancesReportedState.*.isPrimary}", failoverv1.OpContains, "true"),
			cond("{.status.phase}", failoverv1.OpEquals, PhaseHealthy))
		b.declareOthers(r)
		if to.CNPG.Pooler != nil {
			b.scale(to, to.CNPG.Pooler.Instances)
		}
		follow = append(follow, failoverv1.FollowUp{
			Site:   from.Name,
			Action: engine.FollowUpRebuild,
			Reason: fmt.Sprintf("Cluster %s was promoted without a token, so Cluster %s may hold writes that %s lacks and cannot follow it: recreate %s from %s (docs/runbooks/rebuild-old-primary.md)",
				toName, from.CNPG.Cluster.Name, toName, from.CNPG.Cluster.Name, toName),
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
