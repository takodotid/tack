package cnpg

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
