package engine_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// archives makes each site's Cluster archive WAL to its own destination.
func (f *fixture) archives() {
	f.t.Helper()
	for _, s := range []string{"a", "b"} {
		u := &unstructured.Unstructured{Object: f.cluster(s)}
		_ = unstructured.SetNestedField(u.Object, map[string]any{"destinationPath": "s3://wal-" + s}, "spec", "backup", "barmanObjectStore")
		if err := f.clients.Sites[s].Update(context.Background(), u); err != nil {
			f.t.Fatal(err)
		}
	}
}

// #74 row 1, [RFC#0004] Requirement 17: after the promotion, the run waits
// until the new primary archives WAL of its new timeline, in both modes.
func TestPromotionWaitsForArchiving(t *testing.T) {
	for _, mode := range []failoverv1.Mode{failoverv1.ModePlanned, failoverv1.ModeSiteLoss} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t)
			f.archives()
			if mode == failoverv1.ModeSiteLoss {
				f.clients.SetDown("a", true)
			}
			sf, err := f.start("b", mode, engine.Options{})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			st := stepOf(sf, "verify-archiving-b")
			if st.State != failoverv1.StepSucceeded || !strings.Contains(st.Message, "archives WAL of its timeline: last archived 00000002.history") {
				t.Errorf("verify-archiving-b: %+v", st)
			}
			for _, fu := range sf.Status.FollowUps {
				if fu.Action == cnpg.FollowUpCheckArchiving {
					t.Errorf("follow-up %+v", fu)
				}
			}
		})
	}
}

// A new primary that does not archive in time is a warning, with a
// follow-up, and the run goes on: its writes came back.
func TestArchivingThatFailsWarns(t *testing.T) {
	f := newFixture(t)
	f.archives()
	testenv.ArchiveFails = "unexpected failure invoking barman-cloud-wal-archive: exit status 4"
	t.Cleanup(func() { testenv.ArchiveFails = "" })
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run %s, want Succeeded", sf.Status.State)
	}
	st := stepOf(sf, "verify-archiving-b")
	if st.State != failoverv1.StepSucceeded || !strings.HasPrefix(st.Message, "warning: not done after 2s: condition ContinuousArchiving is False (unexpected failure invoking barman-cloud-wal-archive") {
		t.Errorf("verify-archiving-b: %+v", st)
	}
	found := false
	for _, fu := range sf.Status.FollowUps {
		found = found || fu.Site == "b" && fu.Action == cnpg.FollowUpCheckArchiving && strings.Contains(fu.Reason, "ContinuousArchiving is False")
	}
	if !found {
		t.Errorf("follow-ups %+v lack CheckArchiving for site b", sf.Status.FollowUps)
	}
}

// A Cluster without a WAL archive has nothing to check.
func TestNoArchiveNothingToCheck(t *testing.T) {
	f := newFixture(t)
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st := stepOf(sf, "verify-archiving-b"); st.State != failoverv1.StepSucceeded || st.Message != "no WAL archive configured: nothing to check" {
		t.Errorf("verify-archiving-b: %+v", st)
	}
}
