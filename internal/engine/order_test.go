package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

// #38 item 7. A run's creation time comes from the API server of its
// ledger's site, to the second. tack resume of run-1, whose ledger is at
// b, finds another run of the workload, run-9: it refuses when run-9 may
// have started after run-1, also in the same second at the same site, or
// a little earlier by another site's clock. v1.2.1 compared the times with
// a strict "after", across sites.
func TestResumeOrdersRunsAcrossSites(t *testing.T) {
	for _, tt := range []struct {
		name string
		// site is run-9's to site, where its ledger is.
		site string
		// offset is run-9's creation time minus run-1's.
		offset   time.Duration
		takeOver bool
		refused  string
	}{
		{name: "same site, same second", site: "b", refused: "run run-9 started after run run-1, or in the same second"},
		{name: "another site, 5 s before by its clock", site: "a", offset: -5 * time.Second, refused: "or less than 10s before it by the clock of another site's API server"},
		{name: "another site, same second", site: "a", refused: "or less than 10s before it"},
		{name: "same site, 1 s before", site: "b", offset: -time.Second},
		{name: "another site, 1 min before", site: "a", offset: -time.Minute},
		{name: "same site, same second, taken over by run-1", site: "b", takeOver: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			created := time.Now().Truncate(time.Second).Add(-time.Hour)
			other := &failoverv1.SiteFailover{
				ObjectMeta: metav1.ObjectMeta{Name: "run-9", Namespace: "tack-system", CreationTimestamp: metav1.NewTime(created.Add(tt.offset))},
				Spec:       failoverv1.SiteFailoverSpec{Mode: failoverv1.ModeSiteLoss, From: "a", To: tt.site},
				Status:     failoverv1.SiteFailoverStatus{State: failoverv1.RunStopped},
			}
			if err := testenv.CreateLedger(ctx, f.clients.Sites[tt.site], other); err != nil {
				t.Fatal(err)
			}
			var opts engine.Options
			if tt.takeOver {
				opts.TakeOver = []string{"run-9"}
			}
			f.flux.Fail["VerifyGit"] = errors.New("stop here")
			if _, err := f.start("b", failoverv1.ModePlanned, opts); !errors.Is(err, engine.ErrStopped) {
				t.Fatalf("run-1: %v", err)
			}
			delete(f.flux.Fail, "VerifyGit")
			sf := f.ledger("b")
			sf.CreationTimestamp = metav1.NewTime(created)
			if err := f.clients.Sites["b"].Update(ctx, sf); err != nil {
				t.Fatal(err)
			}

			env := f.env("b")
			env.Runs = func(ctx context.Context, c kube.Clients) ([]failoverv1.SiteFailover, error) {
				var out []failoverv1.SiteFailover
				for _, site := range []string{"a", "b"} {
					l, err := (&ledger.Store{Client: f.clients.Sites[site], Namespace: "tack-system"}).List(ctx)
					if err != nil {
						return nil, err
					}
					out = append(out, l...)
				}
				return out, nil
			}
			err := env.Resume(ctx, f.ledger("b"), engine.Options{})
			if tt.refused == "" {
				if sf := f.ledger("b"); err != nil || sf.Status.State != failoverv1.RunSucceeded {
					t.Fatalf("resume: %v, %s: %s", err, sf.Status.State, sf.Status.Message)
				}
				return
			}
			if !errors.Is(err, engine.ErrCannotResume) || !strings.Contains(err.Error(), tt.refused) {
				t.Fatalf("err %v, want a refusal with %q", err, tt.refused)
			}
			if st := stepState(f.ledger("b"), "verify-git-a"); st != failoverv1.StepFailed {
				t.Errorf("the refused resume ran a step: verify-git-a is %s", st)
			}
		})
	}
}

// #66. tack prune orders runs by the same clocks as tack resume: a ledger
// counts as certainly created after another only where the resume of the
// other would refuse it as started after (Requirement 18), and the other
// way round. Where the clocks cannot tell, neither is after the other.
func TestCertainlyAfter(t *testing.T) {
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	ledger := func(offset time.Duration) *failoverv1.SiteFailover {
		return &failoverv1.SiteFailover{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(at.Add(offset))}}
	}
	for _, tt := range []struct {
		name     string
		sameSite bool
		// offset is b's creation time minus a's.
		offset time.Duration
		after  bool
	}{
		{name: "same site, same second", sameSite: true},
		{name: "same site, 1 s later", sameSite: true, offset: time.Second, after: true},
		{name: "same site, 1 s earlier", sameSite: true, offset: -time.Second},
		{name: "another site, same second"},
		{name: "another site, 9 s later", offset: 9 * time.Second},
		{name: "another site, 10 s later", offset: 10 * time.Second, after: true},
		{name: "another site, 1 min later", offset: time.Minute, after: true},
		{name: "another site, 5 s earlier", offset: -5 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, b := ledger(0), ledger(tt.offset)
			if got := engine.CertainlyAfter(b, a, tt.sameSite); got != tt.after {
				t.Errorf("CertainlyAfter(b, a) = %v, want %v", got, tt.after)
			}
			// The complement of the resume's rule: a resume of a refuses
			// b as started after it unless b is certainly before a.
			if engine.CertainlyAfter(b, a, tt.sameSite) && engine.CertainlyAfter(a, b, tt.sameSite) {
				t.Error("each is certainly after the other")
			}
		})
	}
}
