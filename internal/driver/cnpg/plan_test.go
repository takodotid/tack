package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/testenv"
)

// spec3 is a Workload spec with sites a, b and c, each with Cluster
// pg-<site> and Pooler pg-<site>-pooler-rw in namespace db.
func spec3() *failoverv1.WorkloadSpec {
	w := &failoverv1.WorkloadSpec{
		Timeouts: failoverv1.Timeouts{
			Demotion:  &metav1.Duration{Duration: 7 * time.Minute},
			Promotion: &metav1.Duration{Duration: 11 * time.Minute},
		},
	}
	for _, s := range []string{"a", "b", "c"} {
		w.Sites = append(w.Sites, failoverv1.Site{Name: s, KubeContext: "site-" + s, CNPG: &failoverv1.CNPGSite{
			Cluster: failoverv1.ObjectKey{Namespace: "db", Name: "pg-" + s},
			Pooler:  &failoverv1.CNPGPooler{ObjectKey: failoverv1.ObjectKey{Namespace: "db", Name: "pg-" + s + "-pooler-rw"}, Instances: 2},
		}})
	}
	return w
}

// topology reads three sites through Discover. Site a is the primary, b
// and c follow it; edit changes a site's Cluster before the read.
func topology(t *testing.T, edit func(site string, replica, status map[string]any)) (*failoverv1.WorkloadSpec, *driver.Topology) {
	t.Helper()
	objects := map[string][]client.Object{}
	for _, s := range []string{"a", "b", "c"} {
		c := testenv.Cluster(s, "a", s == "a", nil)
		if edit != nil {
			replica := c.Object["spec"].(map[string]any)["replica"].(map[string]any)
			edit(s, replica, c.Object["status"].(map[string]any))
		}
		n := int64(0)
		if s == "a" {
			n = 2
		}
		objects[s] = []client.Object{c, testenv.Pooler(s, n)}
	}
	w := spec3()
	topo, err := Driver{}.Discover(context.Background(), w, testenv.NewClients(objects))
	if err != nil {
		t.Fatal(err)
	}
	return w, topo
}

// RFC0004 Requirements 1, 4 and 5: each refusal of the driver's Plan, with
// its own message. A case at site c is at neither from nor to, so only the
// check of every site can refuse it.
func TestPlanRefusals(t *testing.T) {
	planned := driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}
	siteLoss := driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}
	unhealthy := func(st map[string]any) { st["phase"] = "Setting up primary" }
	for _, tt := range []struct {
		name string
		req  driver.Request
		edit func(site string, replica, status map[string]any)
		want string
	}{
		{name: "planned: from is not primary", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "a" {
					st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
				}
			},
			want: "site a is not a healthy primary"},
		{name: "planned: from is not healthy", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "a" {
					unhealthy(st)
				}
			},
			want: "site a is not a healthy primary"},
		{name: "planned: to is not a replica", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "b" {
					r["primary"] = "pg-b"
					st["instancesReportedState"] = map[string]any{"pg-b-1": map[string]any{"isPrimary": true}}
				}
			},
			want: "site b is not a replica"},
		{name: "planned: to is not healthy", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "b" {
					unhealthy(st)
				}
			},
			want: "site b is not healthy"},
		{name: "planned: a site declares another primary", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "c" {
					r["primary"] = "pg-b"
				}
			},
			want: "site c declares b primary, not a"},
		{name: "planned: to sets minApplyDelay", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "b" {
					r["minApplyDelay"] = "1h"
				}
			},
			want: "site b: Cluster sets spec.replica.minApplyDelay"},
		{name: "planned: a demotion in progress", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "c" {
					st["switchReplicaClusterStatus"] = map[string]any{"inProgress": true}
				}
			},
			want: "site c: a demotion is in progress"},
		{name: "planned: spec.replica.enabled", req: planned,
			edit: func(s string, r, st map[string]any) {
				if s == "c" {
					r["enabled"] = true
				}
			},
			want: "site c: Cluster pg-c uses spec.replica.enabled"},
		{name: "site loss: from answers as primary", req: siteLoss,
			want: "site a answers as primary"},
		{name: "site loss: to is not healthy", req: siteLoss,
			edit: func(s string, r, st map[string]any) {
				if s == "b" {
					unhealthy(st)
				}
				if s == "a" {
					r["primary"] = "pg-b"
					st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
				}
			},
			want: "site b is not healthy"},
		{name: "site loss: spec.replica.enabled", req: siteLoss,
			edit: func(s string, r, st map[string]any) {
				if s == "a" {
					r["primary"] = "pg-b"
					st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
				}
				if s == "c" {
					r["enabled"] = false
				}
			},
			want: "site c: Cluster pg-c uses spec.replica.enabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, topo := topology(t, tt.edit)
			steps, _, err := Driver{}.Plan(w, topo, tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Plan: %v, %d steps; want an error with %q", err, len(steps), tt.want)
			}
		})
	}
}

// The topologies the refusals start from are allowed: each refusal above
// comes from its own change.
func TestPlanAllowed(t *testing.T) {
	w, topo := topology(t, nil)
	if _, _, err := (Driver{}).Plan(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}); err != nil {
		t.Errorf("planned: %v", err)
	}
	w, topo = topology(t, func(s string, r, st map[string]any) {
		if s == "a" {
			r["primary"] = "pg-b"
			st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
		}
	})
	if _, _, err := (Driver{}).Plan(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}); err != nil {
		t.Errorf("site loss: %v", err)
	}
}

// RFC0002 Requirement 2: once Plan allows a request, it returns what Steps
// returns from the Workload alone, and Objects lists what Discover reports.
// A resume composes the plan again from them ([RFC#0003] Requirement 21).
func TestStepsArePlansSteps(t *testing.T) {
	for _, tt := range []struct {
		name string
		req  driver.Request
		edit func(site string, replica, status map[string]any)
	}{
		{name: "planned", req: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}},
		{name: "planned to c", req: driver.Request{From: "a", To: "c", Mode: failoverv1.ModePlanned}},
		{name: "site loss", req: driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss},
			edit: func(s string, r, st map[string]any) {
				if s == "a" {
					r["primary"] = "pg-b"
					st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
				}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, topo := topology(t, tt.edit)
			planned, pfollow, err := Driver{}.Plan(w, topo, tt.req)
			if err != nil {
				t.Fatal(err)
			}
			steps, sfollow, err := Driver{}.Steps(w, tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(planned, steps) || !reflect.DeepEqual(pfollow, sfollow) {
				t.Errorf("Plan and Steps differ:\nPlan  %+v %+v\nSteps %+v %+v", planned, pfollow, steps, sfollow)
			}
			for _, s := range topo.Sites {
				if got := (Driver{}).Objects(w, s.Name); !reflect.DeepEqual(got, s.Objects) {
					t.Errorf("site %s: Objects %v, Discover %v", s.Name, got, s.Objects)
				}
			}
		})
	}
}

// leaves lists the fields a JSON merge patch sets, as dotted paths, with
// their JSON values; null removes a field.
func leaves(t *testing.T, merge string) map[string]string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(merge), &m); err != nil {
		t.Fatalf("patch %s: %v", merge, err)
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if mm, ok := v.(map[string]any); ok {
			for k, c := range mm {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, c)
			}
			return
		}
		b, _ := json.Marshal(v)
		out[prefix] = string(b)
	}
	walk("", m)
	return out
}

// RFC0002 Requirement 2: GitChange covers every field the Failover steps
// patch, with the value the last patch leaves, so Git equals live after
// the run. A field the patches remove is a removal in Git.
func TestGitChangeCoversPatches(t *testing.T) {
	for _, tt := range []struct {
		mode failoverv1.Mode
		edit func(site string, replica, status map[string]any)
	}{
		{mode: failoverv1.ModePlanned},
		{mode: failoverv1.ModeSiteLoss, edit: func(s string, r, st map[string]any) {
			if s == "a" {
				r["primary"] = "pg-b"
				st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
			}
		}},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			w, topo := topology(t, tt.edit)
			req := driver.Request{From: "a", To: "b", Mode: tt.mode}
			steps, _, err := Driver{}.Plan(w, topo, req)
			if err != nil {
				t.Fatal(err)
			}
			changes, err := Driver{}.GitChange(w, topo, req)
			if err != nil {
				t.Fatal(err)
			}
			git := map[string]*string{}
			for _, c := range changes {
				git[c.Site+" "+c.Object.Kind+"/"+c.Object.Name+" "+c.Field] = c.Value
			}
			final := map[string]string{}
			for _, s := range steps {
				if s.Type != failoverv1.StepPatch {
					continue
				}
				for path, v := range leaves(t, s.Patch.Merge) {
					final[s.Site+" "+s.Patch.Object.Kind+"/"+s.Patch.Object.Name+" "+path] = v
				}
			}
			if len(final) == 0 {
				t.Fatal("no patched field")
			}
			for k, v := range final {
				g, ok := git[k]
				switch {
				case !ok:
					t.Errorf("%s is patched to %s, and Git does not set it", k, v)
				case v == "null" && g != nil:
					t.Errorf("%s is removed live, Git sets %s", k, *g)
				case v != "null" && (g == nil || *g != v):
					t.Errorf("%s is %s live after the patches, Git has %v", k, v, g)
				}
			}
		})
	}
}

// RFC0004 Requirement 10: the demotion wait times out after
// timeouts.demotion, every other wait after timeouts.promotion.
func TestWaitTimeouts(t *testing.T) {
	for _, mode := range []failoverv1.Mode{failoverv1.ModePlanned, failoverv1.ModeSiteLoss} {
		w, topo := topology(t, func(s string, r, st map[string]any) {
			if mode == failoverv1.ModeSiteLoss && s == "a" {
				r["primary"] = "pg-b"
				st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
			}
		})
		steps, _, err := Driver{}.Plan(w, topo, driver.Request{From: "a", To: "b", Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		waits := 0
		for _, s := range steps {
			if s.Type != failoverv1.StepWait {
				continue
			}
			waits++
			want := w.Timeouts.Promotion.Duration
			if strings.HasPrefix(s.ID, "wait-demoted-") {
				want = w.Timeouts.Demotion.Duration
			}
			if s.Wait.Timeout.Duration != want {
				t.Errorf("%s %s: timeout %s, want %s", mode, s.ID, s.Wait.Timeout.Duration, want)
			}
		}
		if waits == 0 {
			t.Errorf("%s: no Wait step", mode)
		}
	}
}

// RFC0004 Requirement 9: Verify at to waits for a healthy primary that
// declares itself; at any other site, for its Cluster to declare to's.
func TestVerify(t *testing.T) {
	w := spec3()
	req := driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}
	for site, want := range map[string][]string{
		"b": {`{.spec.replica.primary} Equals "pg-b"`, `{.status.instancesReportedState.*.isPrimary} Contains "true"`, `{.status.phase} Equals "Cluster in healthy state"`},
		"a": {`{.spec.replica.primary} Equals "pg-b"`},
		"c": {`{.spec.replica.primary} Equals "pg-b"`},
	} {
		steps, err := Driver{}.Verify(w, req, site)
		if err != nil || len(steps) != 1 || steps[0].Type != failoverv1.StepWait || steps[0].ID != "verify-"+site || steps[0].Site != site {
			t.Fatalf("site %s: %v, %+v", site, err, steps)
		}
		var got []string
		for _, c := range steps[0].Wait.Conditions {
			got = append(got, fmt.Sprintf("%s %s %q", c.JSONPath, c.Operator, c.Value))
		}
		if strings.Join(got, "; ") != strings.Join(want, "; ") {
			t.Errorf("site %s: conditions %q, want %q", site, got, want)
		}
		if steps[0].Wait.Object.Name != "pg-"+site {
			t.Errorf("site %s: waits on %s", site, steps[0].Wait.Object.Name)
		}
	}
}

// RFC0004 Requirement 1: each site needs a Cluster with its own name.
func TestValidate(t *testing.T) {
	if err := (Driver{}).Validate(spec3()); err != nil {
		t.Fatalf("three Clusters: %v", err)
	}
	for _, tt := range []struct {
		name string
		edit func(w *failoverv1.WorkloadSpec)
		want string
	}{
		{name: "two sites name one Cluster", edit: func(w *failoverv1.WorkloadSpec) { w.Sites[2].CNPG.Cluster.Name = "pg-a" },
			want: `sites a and c name the same Cluster "pg-a"`},
		{name: "no cnpg", edit: func(w *failoverv1.WorkloadSpec) { w.Sites[1].CNPG = nil },
			want: "site b: spec.sites[].cnpg.cluster needs namespace and name"},
		{name: "a Cluster without namespace", edit: func(w *failoverv1.WorkloadSpec) { w.Sites[1].CNPG.Cluster.Namespace = "" },
			want: "site b: spec.sites[].cnpg.cluster needs namespace and name"},
		{name: "a Pooler without name", edit: func(w *failoverv1.WorkloadSpec) { w.Sites[0].CNPG.Pooler.Name = "" },
			want: "site a: spec.sites[].cnpg.pooler needs namespace and name"},
	} {
		w := spec3()
		tt.edit(w)
		if err := (Driver{}).Validate(w); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}

// RFC0002 Requirement 2: the step after which a site no longer runs as
// primary, by the IDs the plans hold.
func TestDemotion(t *testing.T) {
	for _, tt := range []struct {
		mode failoverv1.Mode
		site string
		want string
	}{
		{failoverv1.ModePlanned, "a", "demote-a"},
		{failoverv1.ModeSiteLoss, "a", "fence-a"},
		{failoverv1.ModePlanned, "c", "declare-c"},
		{failoverv1.ModeSiteLoss, "c", "declare-c"},
		{failoverv1.ModePlanned, "b", ""},
	} {
		if got := (Driver{}).Demotion(driver.Request{From: "a", To: "b", Mode: tt.mode}, tt.site); got != tt.want {
			t.Errorf("%s, site %s: %q, want %q", tt.mode, tt.site, got, tt.want)
		}
	}
}

// RFC0002 Requirement 2 and RFC0004 Requirement 7: on three sites and two,
// in both modes, Demotion names a Failover step of the plan at that site
// for every site but to: for from, the patch that declares to's Cluster
// primary before the promotion; for another site, that patch after it. A
// renamed step would make the engine's reads fall back without an error.
func TestDemotionNamesPlannedSteps(t *testing.T) {
	lost := func(s string, r, st map[string]any) {
		if s == "a" {
			r["primary"] = "pg-b"
			st["instancesReportedState"] = map[string]any{"pg-a-1": map[string]any{"isPrimary": false}}
		}
	}
	for _, mode := range []failoverv1.Mode{failoverv1.ModePlanned, failoverv1.ModeSiteLoss} {
		for _, n := range []int{3, 2} {
			var edit func(string, map[string]any, map[string]any)
			if mode == failoverv1.ModeSiteLoss {
				edit = lost
			}
			w, topo := topology(t, edit)
			w.Sites, topo.Sites = w.Sites[:n], topo.Sites[:n]
			req := driver.Request{From: "a", To: "b", Mode: mode}
			steps, _, err := Driver{}.Plan(w, topo, req)
			if err != nil {
				t.Fatalf("%s, %d sites: %v", mode, n, err)
			}
			promotion := -1
			for i, s := range steps {
				if s.Site == "b" {
					promotion = i
					break
				}
			}
			for _, site := range []string{"a", "b", "c"}[:n] {
				id := Driver{}.Demotion(req, site)
				if site == "b" {
					if id != "" {
						t.Errorf("%s, %d sites: Demotion(b) is %q, want none", mode, n, id)
					}
					continue
				}
				i := slices.IndexFunc(steps, func(s failoverv1.Step) bool { return s.ID == id })
				if i < 0 {
					t.Errorf("%s, %d sites: Demotion(%s) is %q, which the plan lacks", mode, n, site, id)
					continue
				}
				s := steps[i]
				if s.Phase != failoverv1.PhaseFailover || s.Type != failoverv1.StepPatch || s.Site != site || s.Patch.Object.Kind != "Cluster" ||
					!strings.Contains(s.Patch.Merge, `"primary":"pg-b"`) {
					t.Errorf("%s, %d sites: Demotion(%s) names %+v, want the patch that declares pg-b at %s", mode, n, site, s, site)
				}
				if before := i < promotion; before != (site == "a") {
					t.Errorf("%s, %d sites: Demotion(%s) is at %d, the promotion at %d", mode, n, site, i, promotion)
				}
			}
		}
	}
}

// Observed on kind in the first three-site e2e: CloudNativePG's webhook
// refused to make pg-b declare pg-c primary, "External cluster pg-c not
// found", in a step after the promotion. A planned plan now refuses, before
// any step, a site whose Cluster does not name the new primary among its
// spec.externalClusters. A site-loss plan notes it instead, with the step
// where the run stops (coordinator review of PR #68: Git, the way to add
// it, may have gone with the lost site). The new primary's own list, and a
// site that gives no answer, are not read.
func TestPlanRefusesPrimaryNotAmongExternalClusters(t *testing.T) {
	without := func(site, name string) func(map[string]*unstructured.Unstructured) {
		return func(cs map[string]*unstructured.Unstructured) {
			var kept []any
			for _, e := range cs[site].Object["spec"].(map[string]any)["externalClusters"].([]any) {
				if e.(map[string]any)["name"] != name {
					kept = append(kept, e)
				}
			}
			cs[site].Object["spec"].(map[string]any)["externalClusters"] = kept
		}
	}
	planned := driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}
	siteLoss := driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}
	for _, tt := range []struct {
		name string
		req  driver.Request
		edit func(map[string]*unstructured.Unstructured)
		down string
		// fenced: a, the lost site, answers, and runs no primary.
		fenced bool
		want   string
		note   string
	}{
		{name: "a third site", req: planned, edit: without("c", "pg-b"),
			want: "site c: Cluster pg-c does not name pg-b among its spec.externalClusters, so CloudNativePG would refuse to make it declare pg-b primary"},
		{name: "from", req: planned, edit: without("a", "pg-b"),
			want: "site a: Cluster pg-a does not name pg-b among its spec.externalClusters"},
		{name: "a third site, in a site loss", req: siteLoss, edit: without("c", "pg-b"), down: "a",
			note: "warning: site c: Cluster pg-c does not name pg-b among its spec.externalClusters, so CloudNativePG refuses to make it declare pg-b primary: the run stops at step declare-c, after the promotion of pg-b, and that step changes nothing. To go on, add pg-b to that list, at the site and in Git, then resume the run"},
		{name: "from that answers, in a site loss", req: siteLoss, edit: without("a", "pg-b"), fenced: true,
			note: "warning: site a: Cluster pg-a does not name pg-b among its spec.externalClusters, so CloudNativePG refuses to make it declare pg-b primary: the run stops at step fence-a, before the promotion of pg-b, and that step changes nothing."},
		{name: "to's own list", req: planned, edit: without("b", "pg-b")},
		{name: "a site that gives no answer", req: siteLoss, edit: without("a", "pg-b"), down: "a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := map[string]*unstructured.Unstructured{}
			objects := map[string][]client.Object{}
			for _, s := range []string{"a", "b", "c"} {
				primary := s == "a" && tt.down == "" && !tt.fenced
				cs[s] = testenv.Cluster(s, "a", primary, nil)
			}
			tt.edit(cs)
			for _, s := range []string{"a", "b", "c"} {
				objects[s] = []client.Object{cs[s], testenv.Pooler(s, 0)}
			}
			c := testenv.NewClients(objects)
			if tt.down != "" {
				c.SetDown(tt.down, true)
			}
			w := spec3()
			topo, err := Driver{}.Discover(context.Background(), w, c)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = Driver{}.Plan(w, topo, tt.req)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err %v, want %q", err, tt.want)
			}
			// Only the notes of Requirement 12: a site loss also notes its RPO (Requirement 13).
			var mine []string
			for _, n := range (Driver{}).Notes(w, topo, tt.req) {
				if strings.Contains(n, "spec.externalClusters") {
					mine = append(mine, n)
				}
			}
			notes := strings.Join(mine, "\n")
			switch {
			case tt.note == "" && notes != "":
				t.Errorf("notes %q, want none", notes)
			case tt.note != "" && !strings.Contains(notes, tt.note):
				t.Errorf("notes %q, want %q", notes, tt.note)
			}
		})
	}
}
