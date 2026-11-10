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

	"github.com/spf13/cobra"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/version"
)

func pruneLong() string {
	return `Prune deletes the ledgers of old runs of a workload, which every search
for runs reads at every site. It deletes only a run that succeeded with no
Deferred step, and keeps:

  - the newest --keep runs of the workload;
  - every abandoned run, and every run that another run took over;
  - every run with a follow-up Rebuild: delete it by hand once the site's
    member was rebuilt;
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
row 16 of the permissions table. The rules, and how to delete a kept run by
hand:
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
		Example: `  # Keep the 20 newest runs of the workload, and those prune must keep
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
	cmd.Flags().IntVar(&keep, "keep", 0, "keep this many of the newest runs, 1 or more")
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
// gives no answer, or a run is not finished.
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
	return decidePrune(found.runs, keep)
}

// decidePrune decides what to keep of runs, which must all be finished.
func decidePrune(found []foundRun, keep int) (*prunePlan, error) {
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
	p := &prunePlan{}
	for i, r := range runs {
		sf := r.sf
		why := ""
		switch {
		case i < keep:
			why = fmt.Sprintf("one of the %d newest", keep)
		case sf.Status.Abandoned != nil:
			why = "abandoned"
		case takenOver(sf, takers):
			why = "taken over"
		case rebuild(sf) != "":
			why = "follow-up Rebuild for site " + rebuild(sf) + ": delete it by hand once that member was rebuilt"
		case marks(sf, byName) != "":
			why = "only its spec marks run " + marks(sf, byName) + " as taken over"
		case version.Newer(sf.Status.WrittenBy, version.Version):
			why = "written by tack " + sf.Status.WrittenBy + ", newer than this tack " + version.Version
		}
		p.runs = append(p.runs, pruneRun{foundRun: r, keep: why})
	}
	return p, nil
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

// rebuild names the sites of the run's follow-ups Rebuild, or "".
func rebuild(sf *failoverv1.SiteFailover) string {
	var sites []string
	for _, f := range sf.Status.FollowUps {
		if f.Action == "Rebuild" {
			sites = append(sites, f.Site)
		}
	}
	return strings.Join(sites, ", ")
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
// one and not the other, or a ledger written in between.
func (p *prunePlan) differs(q *prunePlan) string {
	ids := func(p *prunePlan) map[string]string {
		m := map[string]string{}
		for _, r := range p.runs {
			m[r.site+"/"+r.sf.Name] = string(r.sf.UID) + "@" + r.sf.ResourceVersion + " " + r.keep
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
		case v != a[k]:
			return fmt.Sprintf("the ledger of run %s at site %s was written", r.sf.Name, r.site)
		}
	}
	return ""
}

// print lists the runs, newest first, with what prune does to each.
func (p *prunePlan) print(w io.Writer, workload string, keep int) {
	fmt.Fprintf(w, "Runs of workload %s, newest first; prune keeps the %d newest:\n", workload, keep)
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
		action := "delete"
		if r.keep != "" {
			action = "keep: " + r.keep
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.site, r.sf.Name, state, r.sf.CreationTimestamp.UTC().Format("2006-01-02 15:04:05Z"), action)
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
