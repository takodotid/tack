package engine

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/internal/kube"
)

// Client returns the site's client, whose reads Read repeats ([RFC#0007]
// Requirement 11): every read through SiteChecks, the driver's reads of a
// site and the searches for runs among them. Writes are not repeated.
func (c *SiteChecks) Client(site string) (client.Client, error) {
	cl, err := c.Clients.Client(site)
	if err != nil {
		return nil, err
	}
	return &rereading{Client: cl, site: site, checks: c}, nil
}

// Read runs read, a read of the site's objects, and repeats it while it
// gets no answer, or an answer 5xx or 429 (kube.Transient), with the
// pauses of the site's checks, up to as many reads as the site gets
// checks: 1 when Once names it, else len(Waits)+1 (#64). The site answered
// its check, so such a failure is a blip. Any other error, such as a 403
// or a 404, is returned at once. A read whose tries ran out returns its
// last error, which never counts the site as giving no answer. An
// interrupt during a pause returns ctx's error. Each repeat prints a
// progress line, and so does an answer after one.
func (c *SiteChecks) Read(ctx context.Context, site, what string, read func() error) error {
	waits := c.Waits
	if waits == nil {
		waits = DefaultReachWaits
	}
	tries := len(waits) + 1
	if c.Once != nil && c.Once(site) {
		tries = 1
	}
	for n := 1; ; n++ {
		err := read()
		switch {
		case err == nil:
			if n > 1 {
				c.print(site, "  answered  site %s, %s, on try %d of %d\n", site, what, n, tries)
			}
			return nil
		case ctx.Err() != nil:
			return err
		case !kube.Transient(err):
			return err
		case n >= tries && n > 1:
			return fmt.Errorf("%d reads: %w", n, err)
		case n >= tries:
			return err
		}
		c.print(site, "  retry     site %s, %s, try %d of %d (%v); next try in %s\n", site, what, n, tries, err, waits[n-1])
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waits[n-1]):
		}
	}
}

// rereading is a site's client whose Get and List Read repeats.
type rereading struct {
	client.Client
	site   string
	checks *SiteChecks
}

func (r *rereading) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	what := "read " + kindOf(obj) + " " + key.Name
	if key.Namespace != "" {
		what = "read " + kindOf(obj) + " " + key.Namespace + "/" + key.Name
	}
	return r.checks.Read(ctx, r.site, what, func() error { return r.Client.Get(ctx, key, obj, opts...) })
}

func (r *rereading) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return r.checks.Read(ctx, r.site, "list "+kindOf(list), func() error { return r.Client.List(ctx, list, opts...) })
}

// kindOf names an object's kind: from its type meta, such as an
// unstructured object's, or else from its Go type.
func kindOf(o runtime.Object) string {
	if k := o.GetObjectKind().GroupVersionKind().Kind; k != "" {
		return k
	}
	t := reflect.TypeOf(o)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}
