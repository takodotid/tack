package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/version"
)

func pruneLong() string {
	return `Prune deletes the ledgers of old runs of a workload, which every search
for runs reads at every site. It deletes only a run that succeeded with no
Deferred step, and keeps:

  - the newest --keep runs that succeeded, so the record of the last
    switch stays. Where the clocks of two sites' API servers cannot order
    two runs, created less than 10s apart, prune keeps both;
  - every abandoned run, and every run that another run took over: delete
    one by hand, as the link below shows;
  - every run with a follow-up Rebuild whose member was not created again
    10s or more after the run's ledger was created, as the Workload file
    names the member at that site, or whose member cannot be read: a
    planned run onto that member needs it;
  - every run whose spec records that it took over a run that is kept and
    is not marked as taken over: without it, that run would block new runs;
  - every run that a newer tack than this one last wrote.

Prune acts only when every site of the workload answers and every run of
the workload is finished: succeeded with no Deferred step, abandoned or
taken over. It lists each run with what it does to it, asks for the
workload's name, searches again, and stops, deleting nothing, when anything
changed. It then deletes the ledgers one at a time, by name, oldest first,
each only while it is the object that was read, and stops at the first
error. It changes nothing else.

Prune needs delete on sitefailovers in the ledger namespace at each site,
row 16 of the permissions table, and get on the workload's objects, row 7,
when a run has a follow-up Rebuild. The rules, and how to delete a kept
run by hand:
  ` + version.DocLink("docs/operations.md#old-ledgers") + `

An agent never starts "tack prune" without an explicit instruction from a
human for that workload.`
}

func newPrune(o *options) *cobra.Command {
	var file string
	var keep int
	var yes bool
	cmd := &cobra.Command{
		Use:   "prune -f WORKLOAD --keep N",
		Short: "Delete the ledgers of old runs that succeeded",
		Long:  pruneLong(),
		Example: `  # Keep the 20 newest runs that succeeded, and those prune must keep
  tack prune -f workload.yaml --keep 20`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if file == "" {
				return usagef("pass --file: prune reads every site of the workload")
			}
			if keep < 1 {
				return usagef("pass --keep N, 1 or more: prune keeps the newest runs")
			}
			w, err := o.loadWorkload(file)
			if err != nil {
				return err
			}
			progress := o.progress(out)
			// Every site gets all of its checks, and keeps their result for
			// the search after the confirmation.
			checks := &engine.SiteChecks{Clients: o.factory.Clients(o.kubeconfig, kube.Contexts(&w.Spec)), Waits: o.reachWaits, Out: progress, Now: o.now}
			p, err := planPrune(ctx, checks, &w.Spec, w.Name, keep)
			if err != nil {
				return err
			}
			p.print(out, w.Name, keep)
			del := p.deletes()
			if len(del) == 0 {
				fmt.Fprintln(out, "Nothing to delete.")
				return nil
			}
			if err := o.confirmThat(out, yes, w.Name, "This deletes "+ledgers(len(del))+".", "deleting these ledgers"); err != nil {
				return err
			}
			// A run can start, or a ledger change, while the operator
			// confirms: search again, and delete only what was listed.
			again, err := planPrune(ctx, checks, &w.Spec, w.Name, keep)
			if err != nil {
				return err
			}
			if d := p.differs(again); d != "" {
				return fmt.Errorf("the runs changed since they were listed: %s. Nothing was deleted: prune again", d)
			}
			for i, r := range del {
				cl, err := checks.Client(r.site)
				if err != nil {
					return deleted(i, len(del), err)
				}
				store := &ledger.Store{Client: cl, Namespace: w.Spec.Ledger.Namespace}
				if err := store.Delete(ctx, r.sf); err != nil {
					return deleted(i, len(del), fmt.Errorf("delete the ledger of run %s at site %s: %w", r.sf.Name, r.site, err))
				}
				fmt.Fprintf(progress, "deleted   ledger %s/%s at site %s\n", w.Spec.Ledger.Namespace, r.sf.Name, r.site)
			}
			fmt.Fprintf(out, "Deleted %s of workload %s; kept %d.\n", ledgers(len(del)), w.Name, len(p.runs)-len(del))
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "the Workload file: prune reads every site of the workload")
	cmd.Flags().IntVar(&keep, "keep", 0, "keep this many of the newest runs that succeeded, 1 or more")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation; only when a human approved deleting these ledgers")
	return cmd
}

// deleted is the error of a delete that failed after done deletes.
func deleted(done, of int, err error) error {
	return fmt.Errorf("%w. Deleted %d of %s; the others stay: prune again once the cause is fixed", err, done, ledgers(of))
}

// ledgers counts ledgers: "1 ledger", "2 ledgers".
func ledgers(n int) string {
	if n == 1 {
		return "1 ledger"
	}
	return fmt.Sprintf("%d ledgers", n)
}

// prunePlan is what tack prune does to each run of a workload.
type prunePlan struct {
	// runs are the workload's runs, newest first, each with its reason
	// to stay, or none when it is deleted.
	runs []pruneRun
}

type pruneRun struct {
	foundRun
	keep string
}

// errNotFinished marks a refusal for a run that is not finished.
var errNotFinished = errors.New("not every run of the workload is finished")

// planPrune searches the workload's runs at every site through c, and
// decides what to keep ([RFC#0003] Requirement 27). It fails when a site
// gives no answer, or a run is not finished. It reads the sites' members
// through the workload's driver only when a run that it would otherwise
// delete has a follow-up Rebuild.
func planPrune(ctx context.Context, c kube.Clients, spec *failoverv1.WorkloadSpec, workload string, keep int) (*prunePlan, error) {
	found, err := findRuns(ctx, c, spec, workload)
	if err != nil {
		return nil, fmt.Errorf("%w. Nothing was deleted", err)
	}
	if len(found.missing) > 0 {
		var why []string
		for _, m := range found.missing {
			why = append(why, fmt.Sprintf("site %s: %s", m.site, m.reason()))
		}
		return nil, fmt.Errorf("prune reads every site, and %s gave no answer, so its runs were not read (%s). Nothing was deleted", found.names(), strings.Join(why, "; "))
	}
	var (
		topo    *driver.Topology
		topoErr error
		read    bool
	)
	members := func() (*driver.Topology, error) {
		if !read {
			read = true
			d, err := driver.Get(spec.Driver)
			if err != nil {
				topoErr = err
			} else {
				topo, topoErr = d.Discover(ctx, spec, c)
			}
		}
		return topo, topoErr
	}
	return decidePrune(found.runs, keep, members)
}

// decidePrune decides what to keep of runs, which must all be finished.
// members returns the sites' members as the Workload file names them, as
// tack plan reads them ([RFC#0003] Requirement 14).
func decidePrune(found []foundRun, keep int, members func() (*driver.Topology, error)) (*prunePlan, error) {
	all := make([]failoverv1.SiteFailover, len(found))
	for i, r := range found {
		all[i] = *r.sf
	}
	takers := takers(found)
	runs := slices.Clone(found)
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i].sf, runs[j].sf
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return b.CreationTimestamp.Before(&a.CreationTimestamp)
		}
		return a.Name > b.Name
	})
	var open []string
	for _, r := range runs {
		if !finished(r.sf, takers) {
			open = append(open, fmt.Sprintf("run %s at site %s is %s", r.sf.Name, r.site, unfinishedState(r.sf)))
		}
	}
	if len(open) > 0 {
		return nil, fmt.Errorf("%w: %s. Prune acts only when each run succeeded with no Deferred step, was abandoned or was taken over. Nothing was deleted", errNotFinished, strings.Join(open, "; "))
	}
	byName := map[string]*failoverv1.SiteFailover{}
	for i := range all {
		byName[all[i].Name] = &all[i]
	}
	// The runs that --keep counts: every finished run that is neither
	// abandoned nor taken over succeeded with no Deferred step. They are
	// newest first, as runs is.
	var counted []foundRun
	for _, r := range runs {
		if r.sf.Status.Abandoned == nil && !takenOver(r.sf, takers) {
			counted = append(counted, r)
		}
	}
	p := &prunePlan{}
	for _, r := range runs {
		sf := r.sf
		why := ""
		switch {
		case sf.Status.Abandoned != nil:
			why = "abandoned"
		case takenOver(sf, takers):
			why = "taken over"
		case newest(r, counted, keep) != "":
			why = newest(r, counted, keep)
		case marks(sf, byName) != "":
			why = "only its spec marks run " + marks(sf, byName) + " as taken over"
		case version.Newer(sf.Status.WrittenBy, version.Version) && !version.Named(version.Version):
			why = "written by tack " + sf.Status.WrittenBy + ", a release, and this tack " + version.Version + " names none"
		case version.Newer(sf.Status.WrittenBy, version.Version):
			why = "written by tack " + sf.Status.WrittenBy + ", newer than this tack " + version.Version
		default:
			// Last: it reads the members, which no other rule needs.
			why = openRebuild(sf, members)
		}
		p.runs = append(p.runs, pruneRun{foundRun: r, keep: why})
	}
	return p, nil
}

// newest tells why r, a run that --keep counts, is kept as one of the keep
// newest of counted, or "". The clocks of the sites' API servers order two
// runs only where engine.CertainlyAfter says so: r goes only when keep runs
// of counted were certainly created after it.
func newest(r foundRun, counted []foundRun, keep int) string {
	rank := slices.IndexFunc(counted, func(o foundRun) bool { return o.site == r.site && o.sf.Name == r.sf.Name })
	if rank < keep {
		return fmt.Sprintf("one of the %d newest that succeeded", keep)
	}
	after := 0
	var near *foundRun
	for i := range counted[:rank] {
		o := &counted[i]
		switch {
		case engine.CertainlyAfter(o.sf, r.sf, o.site == r.site):
			after++
		case i < keep && near == nil:
			near = o
		}
		if after >= keep {
			return ""
		}
	}
	switch {
	case near.site == r.site:
		return fmt.Sprintf("may be one of the %d newest that succeeded: created in the same second as run %s", keep, near.sf.Name)
	default:
		return fmt.Sprintf("may be one of the %d newest that succeeded: created less than %s before run %s at site %s, by another site's clock", keep, engine.ClockSkew, near.sf.Name, near.site)
	}
}

// openRebuild tells why a run's follow-ups Rebuild may still be open, or
// "": the member of each such site, as the Workload file names it, was
// created again ClockSkew or more after the run's ledger. Both times come
// from API servers, the member's from its site's and the ledger's
// (metadata.creationTimestamp) from the ledger site's, never from the
// clock of the host that ran the run, which may be behind. A member that
// cannot be read keeps the run, and so does a ledger without a creation
// time.
func openRebuild(sf *failoverv1.SiteFailover, members func() (*driver.Topology, error)) string {
	created := sf.CreationTimestamp.Time
	var why []string
	for _, f := range sf.Status.FollowUps {
		if f.Action != engine.FollowUpRebuild {
			continue
		}
		topo, err := members()
		var st *driver.SiteState
		ok := false
		if err == nil {
			st, ok = topo.Site(f.Site)
		}
		open := ""
		switch {
		case created.IsZero():
			open = "its ledger has no creation time"
		case err != nil:
			open = fmt.Sprintf("its member could not be read (%v)", err)
		case !ok:
			open = "the Workload file has no such site"
		case !st.Reachable:
			open = "the site gave no answer when its member was read"
		case st.Created.IsZero():
			open = "its member's creation time is unknown"
		case st.Created.Before(created.Add(engine.ClockSkew)):
			open = fmt.Sprintf("its member was created at %s, not %s or more after the run's ledger at %s",
				st.Created.UTC().Format(time.RFC3339), engine.ClockSkew, created.UTC().Format(time.RFC3339))
		}
		if open != "" {
			why = append(why, "follow-up Rebuild for site "+f.Site+": "+open)
		}
	}
	return strings.Join(why, "; ")
}

// finished tells whether a run can no longer run a step: it succeeded with
// no Deferred step, was abandoned, or was taken over.
func finished(sf *failoverv1.SiteFailover, takers map[string][]string) bool {
	return sf.Status.Abandoned != nil || takenOver(sf, takers) ||
		sf.Status.State == failoverv1.RunSucceeded && !hasDeferred(sf)
}

func unfinishedState(sf *failoverv1.SiteFailover) string {
	switch s := sf.Status.State; {
	case s == "":
		return "not started"
	case s == failoverv1.RunSucceeded:
		return "Succeeded with a Deferred step"
	case sf.Status.Reason != "":
		return fmt.Sprintf("%s (%s)", s, sf.Status.Reason)
	default:
		return string(s)
	}
}

// marks names a run that sf takes over and that only sf's spec marks as
// taken over: it is listed, not abandoned, and its own status.supersededBy
// is unset. Deleting sf would make that run block new runs again.
func marks(sf *failoverv1.SiteFailover, byName map[string]*failoverv1.SiteFailover) string {
	for _, name := range sf.TakenOver() {
		o, ok := byName[name]
		if ok && o.Name != sf.Name && o.Status.Abandoned == nil && o.Status.SupersededBy == "" {
			return name
		}
	}
	return ""
}

// deletes returns the runs to delete, oldest first.
func (p *prunePlan) deletes() []foundRun {
	var out []foundRun
	for i := len(p.runs) - 1; i >= 0; i-- {
		if p.runs[i].keep == "" {
			out = append(out, p.runs[i].foundRun)
		}
	}
	return out
}

// differs names the first difference between two plans: a run listed in
// one and not the other, a ledger written in between, or a run that prune
// would now treat otherwise.
func (p *prunePlan) differs(q *prunePlan) string {
	type seen struct{ ledger, keep string }
	ids := func(p *prunePlan) map[string]seen {
		m := map[string]seen{}
		for _, r := range p.runs {
			m[r.site+"/"+r.sf.Name] = seen{ledger: string(r.sf.UID) + "@" + r.sf.ResourceVersion, keep: r.keep}
		}
		return m
	}
	a, b := ids(p), ids(q)
	for _, r := range q.runs {
		k := r.site + "/" + r.sf.Name
		if _, ok := a[k]; !ok {
			return fmt.Sprintf("run %s appeared at site %s", r.sf.Name, r.site)
		}
	}
	for _, r := range p.runs {
		k := r.site + "/" + r.sf.Name
		switch v, ok := b[k]; {
		case !ok:
			return fmt.Sprintf("run %s is no longer at site %s", r.sf.Name, r.site)
		case v.ledger != a[k].ledger:
			return fmt.Sprintf("the ledger of run %s at site %s was written", r.sf.Name, r.site)
		case v.keep != a[k].keep:
			return fmt.Sprintf("what prune does to run %s at site %s changed, to %s", r.sf.Name, r.site, action(v.keep))
		}
	}
	return ""
}

// action is what prune does to a run kept for why, or deletes when why is
// "".
func action(why string) string {
	if why == "" {
		return "delete"
	}
	return "keep: " + why
}

// print lists the runs, newest first, with what prune does to each.
func (p *prunePlan) print(w io.Writer, workload string, keep int) {
	fmt.Fprintf(w, "Runs of workload %s, newest first; prune keeps the %d newest that succeeded:\n", workload, keep)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE\tRUN\tSTATE\tCREATED\tPRUNE")
	takers := takers(p.found())
	for _, r := range p.runs {
		state := string(r.sf.Status.State)
		switch {
		case r.sf.Status.Abandoned != nil:
			state = "Abandoned"
		case takenOver(r.sf, takers):
			state = "TakenOver"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.site, r.sf.Name, state, r.sf.CreationTimestamp.UTC().Format("2006-01-02 15:04:05Z"), action(r.keep))
	}
	tw.Flush()
}

func (p *prunePlan) found() []foundRun {
	out := make([]foundRun, len(p.runs))
	for i, r := range p.runs {
		out[i] = r.foundRun
	}
	return out
}
