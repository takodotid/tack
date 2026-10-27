package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/engine"
)

// The annotations of a hold, as the plan prints them.
const (
	annReconcile = "kustomize.toolkit.fluxcd.io/reconcile"
	annHeldBy    = "failover.tako.id/held-by"
)

// printPlan writes the dry run: every step and every change, in order.
func printPlan(w io.Writer, p *engine.Plan) {
	sf := p.SiteFailover
	fmt.Fprintf(w, "Plan for workload %s: %s, site %s to site %s\n", sf.Spec.Workload.Name, sf.Spec.Mode, sf.Spec.From, sf.Spec.To)
	fmt.Fprintf(w, "Ledger: SiteFailover %s/%s at site %s\n\n", sf.Namespace, sf.Name, sf.Spec.To)

	fmt.Fprintln(w, "Sites:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range p.Topology.Sites {
		if !s.Reachable {
			fmt.Fprintf(tw, "  %s\tunreachable\t%s\n", s.Name, s.Error)
			continue
		}
		health := "unhealthy"
		if s.Healthy {
			health = "healthy"
		}
		fmt.Fprintf(tw, "  %s\t%s\tdeclares %s\t%s\t%s\n", s.Name, s.Role, orNone(s.DeclaredPrimary), health, s.Summary)
	}
	tw.Flush()
	fmt.Fprintln(w)

	printSteps(w, sf, false)

	if len(sf.Status.FollowUps) > 0 {
		fmt.Fprintln(w, "\nFollow-ups for the operator:")
		for _, f := range sf.Status.FollowUps {
			fmt.Fprintf(w, "  site %s: %s: %s\n", f.Site, f.Action, f.Reason)
		}
	}
	if len(p.Notes) > 0 {
		fmt.Fprintln(w, "\nNotes:")
		for _, n := range p.Notes {
			fmt.Fprintf(w, "  %s\n", n)
		}
	}
}

// printSteps writes the steps by phase, with their state when withState.
func printSteps(w io.Writer, sf *v1alpha1.SiteFailover, withState bool) {
	state := map[string]v1alpha1.StepStatus{}
	for _, st := range sf.Status.Steps {
		state[st.ID] = st
	}
	n := 0
	for _, phase := range v1alpha1.Phases {
		header := false
		for _, s := range sf.Spec.Steps {
			if s.Phase != phase {
				continue
			}
			if !header {
				fmt.Fprintf(w, "%s\n", phase)
				header = true
			}
			n++
			site := "Git"
			if s.Site != "" {
				site = "site " + s.Site
			}
			mark := ""
			if s.Deferrable {
				mark = " (waits for the site if unreachable)"
			}
			if withState {
				st := state[s.ID]
				fmt.Fprintf(w, "  %2d. [%s] %s  %s%s\n", n, orPending(st.State), s.ID, site, mark)
			} else {
				fmt.Fprintf(w, "  %2d. %s  %s%s\n", n, s.ID, site, mark)
			}
			fmt.Fprintf(w, "      %s\n", s.Description)
			for _, line := range stepDetail(&s, sf.Name) {
				fmt.Fprintf(w, "        %s\n", line)
			}
			if withState {
				if st := state[s.ID]; st.Message != "" {
					fmt.Fprintf(w, "        result: %s\n", st.Message)
				}
			}
		}
	}
}

// stepDetail lists exactly what a step changes or waits for.
func stepDetail(s *v1alpha1.Step, run string) []string {
	switch s.Type {
	case v1alpha1.StepHold:
		return []string{
			fmt.Sprintf("annotate %s: %s=disabled, %s=%s", ref(s.Hold.Object), annReconcile, annHeldBy, run),
			"applied by " + kustomization(s.Hold.Kustomization),
		}
	case v1alpha1.StepRelease:
		return []string{
			fmt.Sprintf("remove from %s: %s, %s", ref(s.Release.Object), annReconcile, annHeldBy),
		}
	case v1alpha1.StepReconcile:
		var ks []string
		for _, k := range s.Reconcile.Kustomizations {
			ks = append(ks, k.Namespace+"/"+k.Name)
		}
		target := "Kustomizations " + strings.Join(ks, ", ")
		if len(ks) == 0 {
			target = "the Kustomizations of the held objects (found when the step runs)"
		}
		line := fmt.Sprintf("request a reconcile of %s and their GitRepository; wait up to %s", target, s.Reconcile.Timeout.Duration)
		out := []string{line}
		if s.Reconcile.Revision != "" {
			out = append(out, "until Flux has applied "+s.Reconcile.Revision)
		}
		return out
	case v1alpha1.StepPatch:
		return []string{fmt.Sprintf("patch %s (merge): %s", ref(s.Patch.Object), s.Patch.Merge)}
	case v1alpha1.StepWait:
		var out []string
		out = append(out, fmt.Sprintf("wait up to %s on %s until:", s.Wait.Timeout.Duration, ref(s.Wait.Object)))
		for _, c := range s.Wait.Conditions {
			out = append(out, "  "+condition(c))
		}
		return out
	case v1alpha1.StepCapture:
		return []string{fmt.Sprintf("record %s of %s in the ledger as $(%s)", s.Capture.JSONPath, ref(s.Capture.Object), s.Capture.Into)}
	case v1alpha1.StepGit:
		var out []string
		for _, c := range s.Git.Changes {
			loc := c.File
			if loc == "" {
				loc = "(file found when Git is reachable)"
			}
			if c.Document != nil {
				loc = fmt.Sprintf("%s, document %d", loc, *c.Document)
			}
			path := c.Path
			if path == "" {
				path = c.Field
			}
			val := "removed"
			if c.Value != nil {
				val = "= " + *c.Value
			}
			out = append(out, fmt.Sprintf("%s: %s %s  (site %s, %s %s)", loc, path, val, c.Site, ref(c.Object), c.Field))
		}
		out = append(out, "commit message: "+firstLine(s.Git.Message))
		return out
	case v1alpha1.StepVerifyGit:
		return []string{fmt.Sprintf("render the run's commit as Flux would and diff it against %d live objects; stop if they differ", len(s.VerifyGit.Objects))}
	case v1alpha1.StepVerifyNoop:
		return []string{fmt.Sprintf("check that the generation of %d released objects did not change", len(s.VerifyNoop.Objects))}
	case v1alpha1.StepRouting:
		return []string{fmt.Sprintf("read SiteService %s/%s until status.route.members is [%s] with reason Active; wait up to %s",
			s.Routing.Service.Namespace, s.Routing.Service.Name, s.Routing.Expected, s.Routing.Timeout.Duration)}
	}
	return nil
}

// printStatus writes a ledger.
func printStatus(w io.Writer, sf *v1alpha1.SiteFailover) {
	st := sf.Status
	fmt.Fprintf(w, "Run %s/%s: workload %s, %s, site %s to site %s\n", sf.Namespace, sf.Name, sf.Spec.Workload.Name, sf.Spec.Mode, sf.Spec.From, sf.Spec.To)
	fmt.Fprintf(w, "State: %s", orPendingRun(st.State))
	if st.Reason != "" {
		fmt.Fprintf(w, " (%s)", st.Reason)
	}
	fmt.Fprintln(w)
	if st.Message != "" {
		fmt.Fprintf(w, "Message: %s\n", st.Message)
	}
	if st.StartedAt != nil {
		fmt.Fprintf(w, "Started: %s\n", st.StartedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if st.FinishedAt != nil {
		fmt.Fprintf(w, "Finished: %s\n", st.FinishedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	g := st.Git
	if g.BaseRevision != "" || g.Commit != "" {
		fmt.Fprintf(w, "Git: base %s", orNone(g.BaseRevision))
		if g.AcceptedRevision != "" {
			fmt.Fprintf(w, ", accepted %s", g.AcceptedRevision)
		}
		fmt.Fprintf(w, ", commit %s\n", orNone(g.Commit))
	}
	if len(st.Captured) > 0 {
		var keys []string
		for k := range st.Captured {
			keys = append(keys, k)
		}
		fmt.Fprintf(w, "Captured: %s\n", strings.Join(sorted(keys), ", "))
	}
	if len(st.Held) > 0 {
		fmt.Fprintln(w, "Held objects:")
		for _, h := range st.Held {
			state := "held"
			if h.ReleasedAt != nil {
				state = fmt.Sprintf("released at generation %d", h.Generation)
			}
			fmt.Fprintf(w, "  site %s: %s (%s)\n", h.Site, ref(h.Object), state)
		}
	}
	fmt.Fprintln(w)
	printSteps(w, sf, true)
	if len(st.FollowUps) > 0 {
		fmt.Fprintln(w, "\nFollow-ups for the operator:")
		for _, f := range st.FollowUps {
			fmt.Fprintf(w, "  site %s: %s: %s\n", f.Site, f.Action, f.Reason)
		}
	}
}

func printRuns(w io.Writer, runs []foundRun) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE\tRUN\tMODE\tFROM\tTO\tSTATE\tREASON\tSTARTED")
	for _, r := range runs {
		sf := r.sf
		started := ""
		if sf.Status.StartedAt != nil {
			started = sf.Status.StartedAt.UTC().Format("2006-01-02 15:04:05Z")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.site, sf.Name, sf.Spec.Mode, sf.Spec.From, sf.Spec.To, orPendingRun(sf.Status.State), sf.Status.Reason, started)
	}
	tw.Flush()
}

func condition(c v1alpha1.Condition) string {
	switch c.Operator {
	case v1alpha1.OpNotEmpty:
		return c.JSONPath + " is not empty"
	case v1alpha1.OpEmpty:
		return c.JSONPath + " is empty"
	}
	return fmt.Sprintf("%s %s %q", c.JSONPath, c.Operator, c.Value)
}

func ref(o v1alpha1.ObjectRef) string { return fmt.Sprintf("%s %s/%s", o.Kind, o.Namespace, o.Name) }

func kustomization(k *v1alpha1.ObjectKey) string {
	if k == nil {
		return "the Flux Kustomization on the object's labels (read when the step runs)"
	}
	return fmt.Sprintf("Flux Kustomization %s/%s", k.Namespace, k.Name)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func orPending(s v1alpha1.StepState) v1alpha1.StepState {
	if s == "" {
		return v1alpha1.StepPending
	}
	return s
}

func orPendingRun(s v1alpha1.RunState) string {
	if s == "" {
		return "NotStarted"
	}
	return string(s)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}
