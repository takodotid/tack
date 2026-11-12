package cli

import (
	"context"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/testenv"
)

// blippingLists fails the first lists of ledgers at a site with errs, in
// order, after the site answered its check.
type blippingLists struct {
	*testenv.Clients
	site  string
	mu    sync.Mutex
	errs  []error
	lists int
}

func (c *blippingLists) Client(site string) (client.Client, error) {
	cl, err := c.Clients.Client(site)
	if err != nil || site != c.site {
		return cl, err
	}
	return &blippingList{Client: cl, c: c}, nil
}

type blippingList struct {
	client.Client
	c *blippingLists
}

func (b *blippingList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	b.c.mu.Lock()
	b.c.lists++
	var err error
	if len(b.c.errs) > 0 {
		err, b.c.errs = b.c.errs[0], b.c.errs[1:]
	}
	b.c.mu.Unlock()
	if err != nil {
		return err
	}
	return b.Client.List(ctx, list, opts...)
}

// #64. A search for runs lists the ledgers at each site that answered its
// check. A 503 on that list, once, is a blip: the list is repeated, and
// tack status lists the site's runs. v1.4.0 failed the search.
func TestSearchRepeatsListAfterBlip(t *testing.T) {
	var lists *blippingLists
	f := &wrapFactory{testFactory: newFactory(statusObjects()), wrap: func(c *testenv.Clients) kube.Clients {
		if lists == nil {
			lists = &blippingLists{Clients: c, site: "b", errs: []error{apierrors.NewServiceUnavailable("etcd is busy")}}
		}
		return lists
	}}
	out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitOK || !strings.Contains(out, "pg-main-earlier") ||
		!strings.Contains(out, "retry     site b, list SiteFailoverList, try 1 of 3 (etcd is busy); next try in 1ms") {
		t.Fatalf("exit %d, want %d with the run and the retry:\n%s", code, ExitOK, out)
	}
	if lists.lists != 2 {
		t.Errorf("%d lists at b, want 2", lists.lists)
	}
}
