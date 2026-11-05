package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/kube"
)

// pager answers a list of SiteFailovers as an API server does: in pages
// when the request sets a limit, from the continue token on. It records
// each request and the JSON size of each answer.
type pager struct {
	requests []client.ListOptions
	answers  []int
	// expire is the request, counted from 1, that the server answers 410
	// Expired, as when the pages' snapshot was compacted. 0 for none.
	expire int
}

func (p *pager) client(objs ...client.Object) client.Client {
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(objs...).Build(), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var o client.ListOptions
			o.ApplyOptions(opts)
			p.requests = append(p.requests, o)
			if len(p.requests) == p.expire {
				return apierrors.NewResourceExpired("the provided continue parameter is too old")
			}
			// The fake filters by namespace and labels, and ignores limit
			// and continue.
			filter := []client.ListOption{client.InNamespace(o.Namespace)}
			if o.LabelSelector != nil {
				filter = append(filter, client.MatchingLabelsSelector{Selector: o.LabelSelector})
			}
			var all failoverv1.SiteFailoverList
			if err := c.List(ctx, &all, filter...); err != nil {
				return err
			}
			sort.Slice(all.Items, func(i, j int) bool { return all.Items[i].Name < all.Items[j].Name })
			start, end, next := 0, len(all.Items), ""
			if o.Continue != "" {
				var err error
				if start, err = strconv.Atoi(o.Continue); err != nil {
					return apierrors.NewBadRequest("bad continue")
				}
			}
			if o.Limit > 0 && start+int(o.Limit) < end {
				end = start + int(o.Limit)
				next = strconv.Itoa(end)
			}
			l := list.(*failoverv1.SiteFailoverList)
			l.Items, l.Continue = all.Items[start:end], next
			b, err := json.Marshal(l)
			if err != nil {
				return err
			}
			p.answers = append(p.answers, len(b))
			return nil
		},
	})
}

// ledgers returns n ledgers of workload, each a copy of base, created a
// minute apart.
func ledgers(base *failoverv1.SiteFailover, workload string, n int) []client.Object {
	start := time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC)
	var out []client.Object
	for i := 0; i < n; i++ {
		sf := base.DeepCopy()
		sf.ObjectMeta = metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%06d", workload, i), Namespace: "tack-system",
			Labels:            map[string]string{"failover.tako.id/workload": workload},
			CreationTimestamp: metav1.NewTime(start.Add(time.Duration(i) * time.Minute)),
		}
		out = append(out, sf)
	}
	return out
}

// #35 row 47. A search listed every ledger of the workload in one
// request. It now reads them in pages of ListPage, each with the same
// namespace and selector, and returns them all, newest first
// ([RFC#0003] Requirement 26).
func TestListInPages(t *testing.T) {
	for _, tt := range []struct {
		name     string
		selector map[string]string
		want     int
		selected string
	}{
		{"search of a workload", map[string]string{"failover.tako.id/workload": "pg-main"}, 250, "failover.tako.id/workload=pg-main"},
		// tack status --context lists every ledger of the namespace.
		{"every ledger of the namespace", nil, 253, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(ledgers(&failoverv1.SiteFailover{}, "pg-main", 250), ledgers(&failoverv1.SiteFailover{}, "pg-other", 3)...)
			p := &pager{}
			s := &Store{Client: p.client(objs...), Namespace: "tack-system", Selector: tt.selector}
			got, err := s.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.want {
				t.Fatalf("%d ledgers, want %d", len(got), tt.want)
			}
			for i := 1; i < len(got); i++ {
				if got[i-1].CreationTimestamp.Before(&got[i].CreationTimestamp) {
					t.Fatalf("ledger %d (%s) is older than ledger %d (%s): want newest first", i-1, got[i-1].Name, i, got[i].Name)
				}
			}
			if want := (tt.want + ListPage - 1) / ListPage; len(p.requests) != want {
				t.Errorf("%d requests, want %d", len(p.requests), want)
			}
			for i, o := range p.requests {
				selected := ""
				if o.LabelSelector != nil {
					selected = o.LabelSelector.String()
				}
				if o.Limit != ListPage || o.Namespace != "tack-system" || selected != tt.selected {
					t.Errorf("request %d: limit %d, namespace %q, selector %q; want limit %d, namespace tack-system, selector %q",
						i+1, o.Limit, o.Namespace, selected, ListPage, tt.selected)
				}
			}
		})
	}
}

// A continue token whose snapshot the API server no longer serves (410
// Expired) makes List read every ledger again in one request, without a
// limit, as client-go's pager does; nothing is counted twice.
func TestListAgainWhenPagesExpire(t *testing.T) {
	p := &pager{expire: 2}
	s := &Store{Client: p.client(ledgers(&failoverv1.SiteFailover{}, "pg-main", 250)...), Namespace: "tack-system",
		Selector: map[string]string{"failover.tako.id/workload": "pg-main"}}
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sf := range got {
		if seen[sf.Name] {
			t.Errorf("ledger %s listed twice", sf.Name)
		}
		seen[sf.Name] = true
	}
	if len(got) != 250 {
		t.Errorf("%d ledgers, want 250", len(got))
	}
	if len(p.requests) != 3 {
		t.Fatalf("%d requests, want 3", len(p.requests))
	}
	if last := p.requests[2]; last.Limit != 0 || last.Continue != "" || last.LabelSelector == nil || last.LabelSelector.String() != "failover.tako.id/workload=pg-main" {
		t.Errorf("the request after 410: limit %d, continue %q, selector %v; want a full list of the workload", last.Limit, last.Continue, last.LabelSelector)
	}
}

// Any other error of a page is the list's error.
func TestListPageError(t *testing.T) {
	p := &pager{expire: 1}
	s := &Store{Client: p.client(ledgers(&failoverv1.SiteFailover{}, "pg-main", 3)...), Namespace: "tack-system"}
	if _, err := s.List(context.Background()); !apierrors.IsResourceExpired(err) {
		t.Errorf("err %v, want the first page's 410", err)
	}
}

// BenchmarkList measures a search's list of n ledgers of a workload at one
// site (#35 row 47): the requests, the largest answer and all answers, in
// bytes of JSON. Each ledger is the planned run of the v1.2.1 testdata, as
// that run left it. Run it with -benchtime=1x.
func BenchmarkList(b *testing.B) {
	raw, err := os.ReadFile("../engine/testdata/ledgers/v1.2.1/planned-mapped.yaml")
	if err != nil {
		b.Fatal(err)
	}
	base := &failoverv1.SiteFailover{}
	if err := yaml.Unmarshal(raw, base); err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("ledgers=%d", n), func(b *testing.B) {
			objs := ledgers(base, "pg-main", n)
			p := &pager{}
			s := &Store{Client: p.client(objs...), Namespace: "tack-system", Selector: map[string]string{"failover.tako.id/workload": "pg-main"}}
			for b.Loop() {
				p.requests, p.answers = nil, nil
				got, err := s.List(context.Background())
				if err != nil || len(got) != n {
					b.Fatalf("%d ledgers, %v", len(got), err)
				}
			}
			largest, total := 0, 0
			for _, a := range p.answers {
				largest = max(largest, a)
				total += a
			}
			b.ReportMetric(float64(len(p.requests)), "requests")
			b.ReportMetric(float64(largest), "largest-answer-bytes")
			b.ReportMetric(float64(total), "answer-bytes")
		})
	}
}
