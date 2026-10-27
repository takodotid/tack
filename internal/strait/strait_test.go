package strait

import (
	"testing"
)

func obj(gen, observed int64, reason string, members ...any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"generation": gen},
		"status": map[string]any{
			"observedGeneration": observed,
			"route":              map[string]any{"members": members, "reason": reason},
		},
	}
}

func TestRouted(t *testing.T) {
	tests := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"routed", obj(2, 2, ReasonActive, "pg-b"), true},
		{"old member", obj(2, 2, ReasonActive, "pg-a"), false},
		{"nowhere", obj(2, 2, "Transitioning"), false},
		{"two members", obj(2, 2, ReasonActive, "pg-a", "pg-b"), false},
		{"stale status", obj(3, 2, ReasonActive, "pg-b"), false},
		{"no status", map[string]any{"metadata": map[string]any{"generation": int64(1)}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := Routed(tt.obj, "pg-b")
			if got != tt.want {
				t.Fatalf("Routed = %v (%s), want %v", got, why, tt.want)
			}
		})
	}
}
