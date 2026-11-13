package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/redact"
	"github.com/takodotid/tack/internal/version"
)

// The annotations of a hold, as the plan prints them.
const (
	annReconcile = "kustomize.toolkit.fluxcd.io/reconcile"
	annHeldBy    = "failover.tako.id/held-by"
)

// printPlan writes the dry run: every step and every change, in order. c
// gives the API server of each site's context.
func printPlan(w io.Writer, p *engine.Plan, c kube.Clients) {
	sf := p.SiteFailover
	fmt.Fprintf(w, "Plan for workload %s: %s, site %s to site %s\n", sf.Spec.Workload.Name, sf.Spec.Mode, sf.Spec.From, sf.Spec.To)
	fmt.Fprintf(w, "Ledger: SiteFailover %s/%s at site %s\n\n", sf.Namespace, sf.Name, sf.Spec.To)

	fmt.Fprintln(w, "Sites:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range p.Topology.Sites {
		if !s.Reachable {
			fmt.Fprintf(tw, "  %s\tunreachable\t%s\n", s.Name, redact.Text(s.Error))
			continue
		}
		health := "unhealthy"
		if s.Healthy {
			health = "healthy"
		}
		fmt.Fprintf(tw, "  %s\t%s\tdeclares %s\t%s\t%s\n", s.Name, s.Role, orNone(s.DeclaredPrimary), health, redact.Text(s.Summary))
	}
	tw.Flush()
	printPositions(w, p.Topology)
	fmt.Fprintln(w)
	printClusters(w, sf, c)
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
			fmt.Fprintf(w, "  %s\n", redact.Text(n))
		}
	}
}

// serverer is a kube.Clients that knows each context's API server.
type serverer interface {
	Server(site string) (string, error)
}

// printClusters writes what a run acts on: per site, its context, the API
// server the context points at in this kubeconfig, and the cluster the run
// recorded; then the Git repository, without credentials.
func printClusters(w io.Writer, sf *failoverv1.SiteFailover, c kube.Clients) {
	fmt.Fprintln(w, "Clusters:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	spec := &sf.Spec.Workload.Spec
	for _, s := range spec.Sites {
		server := "unknown"
		if sv, ok := c.(serverer); ok {
			if u, err := sv.Server(s.Name); err != nil {
				server = "unknown (" + err.Error() + ")"
			} else {
				server = u
			}
		}
		fp, ok := engine.Fingerprint(sf, s.Name)
		if !ok {
			fp = "not read yet"
		}
		fmt.Fprintf(tw, "  site %s\tcontext %s\tserver %s\tkube-system UID %s\n", s.Name, s.KubeContext, redact.Text(server), fp)
	}
	tw.Flush()
	fmt.Fprintf(w, "Git: %s, branch %s\n", redact.Text(spec.Git.URL), spec.Git.Branch)
}

// printSteps writes the steps by phase, with their state when withState:
// then each phase's and each step's start and end, and each step's
// attempts and last result ([RFC#0009] Requirement 28).
func printSteps(w io.Writer, sf *failoverv1.SiteFailover, withState bool) {
	state := map[string]failoverv1.StepStatus{}
	for _, st := range sf.Status.Steps {
		state[st.ID] = st
	}
	phases := map[failoverv1.Phase]failoverv1.PhaseStatus{}
	for _, p := range sf.Status.Phases {
		phases[p.Name] = p
	}
	n := 0
	for _, phase := range failoverv1.Phases {
		header := false
		for _, s := range sf.Spec.Steps {
			if s.Phase != phase {
				continue
			}
			if !header {
				fmt.Fprintf(w, "%s", phase)
				if p, ok := phases[phase]; withState && ok {
					if t := times(p.StartedAt, p.FinishedAt); t != "" {
						fmt.Fprintf(w, "  (%s)", t)
					}
				}
				fmt.Fprintln(w)
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
			for _, line := range stepDetail(&s, sf.Name, sf.Spec.Mode) {
				fmt.Fprintf(w, "        %s\n", line)
			}
			if withState {
				st := state[s.ID]
				if t := times(st.StartedAt, st.FinishedAt); t != "" || st.Attempts > 0 {
					fmt.Fprintf(w, "        %s\n", strings.TrimPrefix(t+fmt.Sprintf(", attempts %d", st.Attempts), ", "))
				}
				if st.Message != "" {
					fmt.Fprintf(w, "        result: %s\n", engine.Clean(sf.Status.Captured, st.Message))
				}
				for i, a := range st.Earlier {
					fmt.Fprintf(w, "        earlier attempt %d: %s, %s", i+1, a.State, times(a.StartedAt, a.FinishedAt))
					if a.Message != "" {
						fmt.Fprintf(w, ": %s", engine.Clean(sf.Status.Captured, a.Message))
					}
					fmt.Fprintln(w)
				}
			}
		}
	}
}

// stepDetail lists exactly what a step changes or waits for.
func stepDetail(s *failoverv1.Step, run string, mode failoverv1.Mode) []string {
	switch s.Type {
	case failoverv1.StepHold:
		return []string{
			fmt.Sprintf("annotate %s: %s=disabled, %s=%s", ref(s.Hold.Object), annReconcile, annHeldBy, run),
			"applied by " + kustomization(s.Hold.Kustomization),
		}
	case failoverv1.StepRelease:
		return []string{
			fmt.Sprintf("remove from %s: %s, %s", ref(s.Release.Object), annReconcile, annHeldBy),
		}
	case failoverv1.StepReconcile:
		var ks []string
		for _, k := range s.Reconcile.Kustomizations {
			ks = append(ks, k.Namespace+"/"+k.Name)
		}
		target := "Kustomizations " + strings.Join(ks, ", ")
		if len(ks) == 0 {
			target = "the Kustomizations of the held objects (found when the step runs)"
		}
		line := fmt.Sprintf("request a reconcile of %s and their GitRepository; wait up to %s", target, s.Reconcile.Timeout.Duration)
		if !engine.FetchesGit(mode, s.Phase) {
			line = fmt.Sprintf("request a reconcile of %s on the artifact Flux has, without a Git fetch; wait up to %s", target, s.Reconcile.Timeout.Duration)
		}
		out := []string{line}
		switch s.Reconcile.Revision {
		case "":
		case engine.GitCommitRef:
			out = append(out, "until Flux has applied the branch head, which must contain the run's commit "+engine.GitCommitRef)
		default:
			out = append(out, "until Flux has applied "+s.Reconcile.Revision)
		}
		return out
	case failoverv1.StepPatch:
		return []string{fmt.Sprintf("patch %s (merge): %s", ref(s.Patch.Object), s.Patch.Merge)}
	case failoverv1.StepWait:
		var out []string
		out = append(out, fmt.Sprintf("wait up to %s on %s until:", s.Wait.Timeout.Duration, ref(s.Wait.Object)))
		for _, c := range s.Wait.Conditions {
			out = append(out, "  "+condition(c))
		}
		return out
	case failoverv1.StepCapture:
		return []string{fmt.Sprintf("record %s of %s in the ledger as $(%s)", s.Capture.JSONPath, ref(s.Capture.Object), s.Capture.Into)}
	case failoverv1.StepGit:
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
	case failoverv1.StepVerifyGit:
		return []string{
			"stop if the branch moved past the run's commit",
			fmt.Sprintf("render the run's commit as Flux would and diff it against %d live objects; stop if they differ", len(s.VerifyGit.Objects)),
		}
	case failoverv1.StepVerifyNoop:
		return []string{fmt.Sprintf("check that the generation of %d released objects did not change", len(s.VerifyNoop.Objects))}
	case failoverv1.StepCreate:
		return []string{
			fmt.Sprintf("create %s, labelled %s=%s: %s", ref(named(s.Create.Object, run)), engine.LabelRun, run, strings.ReplaceAll(s.Create.Manifest, "$(run)", run)),
			"a step that runs again keeps the object this run created, and stops on any other of that name",
		}
	case failoverv1.StepRecord:
		read := ref(named(s.Record.Object, run))
		if s.Record.What == "Position" || s.Record.What == "Fork" || s.Record.What == "Archiving" {
			read += " and its current primary's status, through the API server"
		}
		line := fmt.Sprintf("read %s, change nothing, and record %s", read, recordWhat(s.Record.What))
		if s.Record.Timeout != nil {
			line += fmt.Sprintf("; wait up to %s", s.Record.Timeout.Duration)
		}
		if s.Record.WarnAs != "" {
			line += fmt.Sprintf(", then warn, record the follow-up %s, and go on", s.Record.WarnAs)
		}
		return []string{line}
	case failoverv1.StepRouting:
		return []string{fmt.Sprintf("read SiteService %s/%s until status.route.members is [%s] with reason Active; wait up to %s",
			s.Routing.Service.Namespace, s.Routing.Service.Name, s.Routing.Expected, s.Routing.Timeout.Duration)}
	}
	return nil
}

// printStatus writes a ledger.
func printStatus(w io.Writer, sf *failoverv1.SiteFailover) {
	st := sf.Status
	fmt.Fprintf(w, "Run %s/%s: workload %s, %s, site %s to site %s\n", sf.Namespace, sf.Name, sf.Spec.Workload.Name, sf.Spec.Mode, sf.Spec.From, sf.Spec.To)
	fmt.Fprintf(w, "State: %s", orPendingRun(st.State))
	if st.Reason != "" {
		fmt.Fprintf(w, " (%s)", st.Reason)
	}
	fmt.Fprintln(w)
	if st.Message != "" {
		fmt.Fprintf(w, "Message: %s\n", engine.Clean(st.Captured, st.Message))
	}
	switch runs := sf.TakenOver(); len(runs) {
	case 0:
	case 1:
		fmt.Fprintf(w, "Takes over: run %s\n", runs[0])
	default:
		fmt.Fprintf(w, "Takes over: runs %s\n", strings.Join(runs, ", "))
	}
	if st.SupersededBy != "" {
		fmt.Fprintf(w, "Taken over by: run %s; this run cannot resume\n", st.SupersededBy)
	}
	if a := st.Abandoned; a != nil {
		fmt.Fprintf(w, "Abandoned: %s, %s; this run cannot resume\n", a.At.UTC().Format("2006-01-02 15:04:05Z"), engine.Clean(st.Captured, a.Reason))
	}
	if st.StartedAt != nil {
		fmt.Fprintf(w, "Started: %s\n", st.StartedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if st.FinishedAt != nil {
		fmt.Fprintf(w, "Finished: %s\n", st.FinishedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if st.WrittenBy != "" {
		fmt.Fprintf(w, "Written by: tack %s", st.WrittenBy)
		switch {
		case !version.Newer(st.WrittenBy, version.Version):
		case !version.Named(version.Version):
			fmt.Fprintf(w, ", a release, and this tack %s names none: tack resume and tack abandon refuse the run, and this output may leave out fields this tack does not know", version.Version)
		default:
			fmt.Fprintf(w, ", newer than this tack %s: tack resume and tack abandon refuse the run, and this output leaves out the fields this tack does not know", version.Version)
		}
		fmt.Fprintln(w)
	}
	g := st.Git
	if g.BaseRevision != "" || g.Commit != "" {
		fmt.Fprintf(w, "Git: base %s", orNone(g.BaseRevision))
		if g.AcceptedRevision != "" {
			fmt.Fprintf(w, ", accepted %s", g.AcceptedRevision)
		}
		fmt.Fprintf(w, ", commit %s", orNone(g.Commit))
		if g.ReleaseRevision != "" {
			fmt.Fprintf(w, ", release checked against %s", g.ReleaseRevision)
		}
		if g.ChangedRevision != "" {
			fmt.Fprintf(w, "; the stop names %s for --accept-git-head", g.ChangedRevision)
		}
		fmt.Fprintln(w)
	}
	if line := writesDown(st.Writes); line != "" {
		fmt.Fprintln(w, line)
	}
	if f := st.Fork; f != nil {
		fmt.Fprintf(w, "Fork: site %s's new history leaves the old one at %s", f.Site, orNone(f.Position))
		if f.ParentTimeline != 0 {
			fmt.Fprintf(w, " on timeline %d", f.ParentTimeline)
		}
		if f.Position != "" {
			fmt.Fprint(w, " or just after")
		}
		if f.Timeline != 0 {
			fmt.Fprintf(w, "; new timeline %d", f.Timeline)
		}
		if f.At != nil {
			fmt.Fprintf(w, "; promoted by %s", at(*f.At))
		}
		if f.Message != "" {
			fmt.Fprintf(w, "; %s", engine.Clean(st.Captured, f.Message))
		}
		fmt.Fprintln(w)
	}
	if len(st.Captured) > 0 {
		var keys []string
		for k := range st.Captured {
			keys = append(keys, k)
		}
		fmt.Fprintf(w, "Captured: %s\n", strings.Join(sorted(keys), ", "))
	}
	if len(st.Held) > 0 {
		// The ledger is not written when a hold is removed by hand (#59).
		fmt.Fprintln(w, "Held objects, as the ledger records them:")
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
			if sn := f.Snapshot; sn != nil {
				fmt.Fprintf(w, "    snapshot: Backup %s/%s, VolumeSnapshots %s", sn.Backup.Namespace, sn.Backup.Name, orNone(strings.Join(sn.VolumeSnapshots, ", ")))
				if sn.At != nil {
					fmt.Fprintf(w, ", recorded %s", at(*sn.At))
				}
				fmt.Fprintln(w)
			}
			if f.Archive != "" {
				fmt.Fprintf(w, "    WAL archive, to keep: %s\n", f.Archive)
			}
		}
	}
	printHistory(w, sf)
}

// printHistory writes the run's history, oldest first: per entry, its
// time, state and reason, the version, user and flags of the tack that
// recorded it, then its message.
func printHistory(w io.Writer, sf *failoverv1.SiteFailover) {
	st := sf.Status
	if len(st.History) == 0 {
		return
	}
	fmt.Fprintln(w, "\nHistory:")
	for i, h := range st.History {
		if i == 1 && st.HistoryDropped > 0 {
			fmt.Fprintf(w, "  ... %d entries dropped to keep the history within %d entries\n", st.HistoryDropped, failoverv1.MaxHistory)
		}
		state := string(h.State)
		if h.Reason != "" {
			state += " (" + h.Reason + ")"
		}
		user := h.User
		if user == "" {
			user = "unknown"
		}
		fmt.Fprintf(w, "  %s  %s  tack %s, user %s", at(h.Time), state, orNone(h.Version), user)
		if len(h.Flags) > 0 {
			fmt.Fprintf(w, ", %s", strings.Join(h.Flags, " "))
		}
		fmt.Fprintln(w)
		if h.Message != "" {
			fmt.Fprintf(w, "      %s\n", engine.Clean(st.Captured, h.Message))
		}
	}
}

// at formats a time of the ledger in UTC.
func at(t metav1.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") }

// times says when something started and finished, as far as recorded.
func times(started, finished *metav1.Time) string {
	var parts []string
	if started != nil {
		parts = append(parts, "started "+at(*started))
	}
	if finished != nil {
		parts = append(parts, "finished "+at(*finished))
	}
	return strings.Join(parts, ", ")
}

func printRuns(w io.Writer, runs []foundRun) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE\tRUN\tMODE\tFROM\tTO\tSTATE\tREASON\tSTARTED")
	takers := takers(runs)
	for _, r := range runs {
		sf := r.sf
		started := ""
		if sf.Status.StartedAt != nil {
			started = sf.Status.StartedAt.UTC().Format("2006-01-02 15:04:05Z")
		}
		state := orPendingRun(sf.Status.State)
		switch {
		case sf.Status.Abandoned != nil:
			state = "Abandoned"
		case takenOver(sf, takers):
			// Its own state is the one it stopped in; it can never go on.
			state = "TakenOver"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.site, sf.Name, sf.Spec.Mode, sf.Spec.From, sf.Spec.To, state, sf.Status.Reason, started)
	}
	tw.Flush()
}

// takers maps each run that a listed run takes over to the runs that take
// it over, so the list reads each run's spec once, not once per run (#35
// row 47).
func takers(runs []foundRun) map[string][]string {
	out := map[string][]string{}
	for _, r := range runs {
		for _, old := range r.sf.TakenOver() {
			out[old] = append(out[old], r.sf.Name)
		}
	}
	return out
}

// takenOver tells whether another run took sf over, as sf's status or the
// spec of a listed run says: takers holds those specs.
func takenOver(sf *failoverv1.SiteFailover, takers map[string][]string) bool {
	if sf.Status.SupersededBy != "" {
		return true
	}
	for _, by := range takers[sf.Name] {
		if by != sf.Name {
			return true
		}
	}
	return false
}

func condition(c failoverv1.Condition) string {
	switch c.Operator {
	case failoverv1.OpNotEmpty:
		return c.JSONPath + " is not empty"
	case failoverv1.OpEmpty:
		return c.JSONPath + " is empty"
	}
	return fmt.Sprintf("%s %s %q", c.JSONPath, c.Operator, c.Value)
}

func ref(o failoverv1.ObjectRef) string { return fmt.Sprintf("%s %s/%s", o.Kind, o.Namespace, o.Name) }

func kustomization(k *failoverv1.ObjectKey) string {
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

func orPending(s failoverv1.StepState) failoverv1.StepState {
	if s == "" {
		return failoverv1.StepPending
	}
	return s
}

func orPendingRun(s failoverv1.RunState) string {
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

// printPositions writes how far each site's data reaches, as its database
// reports it, when the driver reads it ([RFC#0009] Requirement 2).
func printPositions(w io.Writer, t *driver.Topology) {
	read := false
	for _, s := range t.Sites {
		read = read || s.Position != nil
	}
	if !read {
		return
	}
	fmt.Fprintln(w, "\nWAL positions:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range t.Sites {
		p := s.Position
		switch {
		case !s.Reachable:
			fmt.Fprintf(tw, "  %s\tnot read: the site is unreachable\n", s.Name)
		case p == nil:
			fmt.Fprintf(tw, "  %s\tnot read\n", s.Name)
		case p.Error != "":
			fmt.Fprintf(tw, "  %s\tnot read: %s\n", s.Name, redact.Text(p.Error))
		case p.Current != "":
			fmt.Fprintf(tw, "  %s\twrites at %s\t\ttimeline %d\t%s\n", s.Name, p.Current, p.Timeline, p.Standing)
		default:
			stream := "not streaming"
			if p.ReceiverActive {
				stream = "streaming"
			}
			if p.ReplayPaused {
				stream += ", replay paused"
			}
			fmt.Fprintf(tw, "  %s\treceived %s\treplayed %s\ttimeline %d, %s\t%s\n", s.Name, orNone(p.Received), orNone(p.Replayed), p.Timeline, stream, p.Standing)
		}
	}
	tw.Flush()
}

// named is ref with $(run) in its name replaced by the run's name.
func named(ref failoverv1.ObjectRef, run string) failoverv1.ObjectRef {
	ref.Name = strings.ReplaceAll(ref.Name, "$(run)", run)
	return ref
}

// recordWhat says what a Record step records.
func recordWhat(what string) string {
	switch what {
	case "Position":
		return "its WAL position in status.fork: where the new history leaves the old one, or just before"
	case "Fork":
		return "its new timeline in status.fork; a position or timeline that cannot be read is recorded as such, and the run goes on"
	case "Snapshot":
		return "the Backup's VolumeSnapshots and the Cluster's WAL archive on the follow-up Rebuild, once the Backup completes; a failed Backup stops the run"
	case "Archive":
		return "the Cluster's WAL archive on the follow-up Rebuild"
	case "Archiving":
		return "nothing until it archives WAL of its new timeline: condition ContinuousArchiving True, and its last archived file of that timeline; at once without a WAL archive"
	}
	return what
}

// writesDown says when the run's workload took no writes ([RFC#0003]
// Requirement 29).
func writesDown(wr *failoverv1.WriteWindow) string {
	if wr == nil || wr.StoppedAt == nil {
		return ""
	}
	from := at(*wr.StoppedAt)
	if wr.StoppedEstimated {
		from = "by " + from + " at the latest (the run's start: the site was lost before)"
	}
	if wr.ResumedAt == nil {
		return "Writes down: since " + from + "; the new primary does not take writes yet"
	}
	d := wr.ResumedAt.Sub(wr.StoppedAt.Time).Round(time.Second)
	if wr.StoppedEstimated {
		return fmt.Sprintf("Writes down: %s, until %s: at least %s", from, at(*wr.ResumedAt), d)
	}
	return fmt.Sprintf("Writes down: %s to %s (%s)", from, at(*wr.ResumedAt), d)
}
