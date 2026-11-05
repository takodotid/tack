package v1

import (
	"slices"
	"testing"
)

// RFC0003 Requirement 17: the runs a run takes over, from a ledger of
// v1.2.x (takeOver only) and of v1.3.0 (takeOver and takeOvers).
func TestTakenOver(t *testing.T) {
	for _, tt := range []struct {
		spec SiteFailoverSpec
		want []string
	}{
		{SiteFailoverSpec{}, nil},
		{SiteFailoverSpec{TakeOver: "run-1"}, []string{"run-1"}},
		{SiteFailoverSpec{TakeOver: "run-1", TakeOvers: []string{"run-1", "run-2"}}, []string{"run-1", "run-2"}},
		{SiteFailoverSpec{TakeOvers: []string{"run-2", "run-3"}}, []string{"run-2", "run-3"}},
	} {
		sf := &SiteFailover{Spec: tt.spec}
		if got := sf.TakenOver(); !slices.Equal(got, tt.want) {
			t.Errorf("%+v: %v, want %v", tt.spec, got, tt.want)
		}
		for _, r := range tt.want {
			if !sf.TakesOver(r) {
				t.Errorf("%+v: does not take over %s", tt.spec, r)
			}
		}
		if sf.TakesOver("") || sf.TakesOver("run-9") {
			t.Errorf("%+v: takes over an empty name or run-9", tt.spec)
		}
	}
}
