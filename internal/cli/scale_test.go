package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// finishedRuns returns n finished runs at site b, a minute apart.
func finishedRuns(n int) []foundRun {
	start := time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC)
	runs := make([]foundRun, n)
	for i := range runs {
		sf := &failoverv1.SiteFailover{}
		sf.Name = fmt.Sprintf("pg-main-%06d", i)
		t := metav1.NewTime(start.Add(time.Duration(i) * time.Minute))
		sf.CreationTimestamp, sf.Status.StartedAt = t, &t
		sf.Spec.Mode, sf.Spec.From, sf.Spec.To = failoverv1.ModePlanned, "a", "b"
		sf.Status.State = failoverv1.RunSucceeded
		runs[i] = foundRun{site: "b", sf: sf}
	}
	return runs
}

// #35 row 47. tack status -f read every run's spec for each run it
// listed: 2.8 s for 10,000 runs on the test host, 28 ms for 1,000. It now
// reads each spec once. The bound is far above the linear list's time,
// and below the old one's; ten times more with the race detector.
func TestStatusListIsLinear(t *testing.T) {
	runs := finishedRuns(10000)
	// A run another run's spec.takeOvers names, and a run whose own spec
	// names itself, which is not a take-over.
	runs[10].sf.Status.State = failoverv1.RunStopped
	runs[20].sf.Spec.TakeOvers = []string{"pg-main-000005", "pg-main-000010"}
	runs[30].sf.Spec.TakeOver = "pg-main-000030"
	var out bytes.Buffer
	start := time.Now()
	printRuns(&out, runs)
	bound := time.Second
	if raceBuild {
		bound *= 10
	}
	if d := time.Since(start); d > bound {
		t.Errorf("listing 10,000 runs took %s, want less than %s", d, bound)
	}
	for _, tt := range []struct{ run, state string }{
		{"pg-main-000010", "TakenOver"}, {"pg-main-000005", "TakenOver"}, {"pg-main-000030", "Succeeded"}, {"pg-main-000020", "Succeeded"},
	} {
		found := false
		for _, line := range strings.Split(out.String(), "\n") {
			if f := strings.Fields(line); len(f) > 5 && f[1] == tt.run {
				found = true
				if f[5] != tt.state {
					t.Errorf("run %s listed as %s, want %s", tt.run, f[5], tt.state)
				}
			}
		}
		if !found {
			t.Errorf("run %s not listed", tt.run)
		}
	}
}

// BenchmarkStatusList measures what tack status -f does with the runs its
// search found: the list it prints (#35 row 47).
func BenchmarkStatusList(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("runs=%d", n), func(b *testing.B) {
			runs := finishedRuns(n)
			for b.Loop() {
				printRuns(io.Discard, runs)
			}
		})
	}
}
