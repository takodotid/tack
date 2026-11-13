package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/redact"
)

type ledgerFlags struct {
	file      string
	context   string
	namespace string
}

func (l *ledgerFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&l.file, "file", "f", "", "the Workload file: search every site of the workload for its runs")
	cmd.Flags().StringVar(&l.context, "context", "", "a kube context to read runs from, instead of --file")
	cmd.Flags().StringVar(&l.namespace, "namespace", config.DefaultLedgerNamespace, "the ledger namespace, with --context")
}

type foundRun struct {
	site string
	sf   *failoverv1.SiteFailover
}

// contextSite names the single site behind --context.
const contextSite = "context"

// find returns the runs the flags point at, newest first per site, and the
// sites that gave no answer. With --file it checks each site of the
// workload ([RFC#0007] Requirement 5). When name is set, a site that fails
// its first check gets the rest of its checks only if the run is not found
// at another site: a resume of a site-loss run finds its ledger at the to
// site, and does not wait on the lost site. Progress lines go to progress.
func (l *ledgerFlags) find(ctx context.Context, o *options, name string, progress io.Writer) (*search, error) {
	switch {
	case l.file != "" && l.context != "":
		return nil, usagef("pass --file or --context, not both")
	case l.file != "":
		w, err := o.loadWorkload(l.file)
		if err != nil {
			return nil, err
		}
		checks := &engine.SiteChecks{Clients: o.factory.Clients(o.kubeconfig, kube.Contexts(&w.Spec)), Waits: o.reachWaits, Out: progress, Now: o.now}
		if name != "" {
			checks.Once = func(string) bool { return true }
			s, err := findRuns(ctx, checks, &w.Spec, w.Name)
			if err != nil {
				return nil, err
			}
			if _, err := s.pick(name); err == nil {
				return s, nil
			}
			checks.Once = nil
		}
		return findRuns(ctx, checks, &w.Spec, w.Name)
	case l.context != "":
		c := o.factory.Clients(o.kubeconfig, map[string]string{contextSite: l.context})
		cl, err := c.Client(contextSite)
		if err != nil {
			return nil, err
		}
		store := &ledger.Store{Client: cl, Namespace: l.namespace}
		items, err := store.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("context %s: %w", l.context, err)
		}
		s := &search{}
		for i := range items {
			s.runs = append(s.runs, foundRun{site: "(" + l.context + ")", sf: &items[i]})
		}
		return s, nil
	}
	return nil, usagef("pass --file or --context")
}

// search is the result of a search for runs.
type search struct {
	// runs are the runs found, in the order of the Workload's sites.
	runs []foundRun
	// missing are the sites that gave no answer, in the same order: their
	// runs were not read ([RFC#0009] Requirement 16).
	missing []missingSite
}

// missingSite is a site that a search left out: it gave no answer.
type missingSite struct {
	site string
	err  error
}

// reason is the site's error on one line, without a URL's user or
// password ([RFC#0009] Requirement 10).
func (m missingSite) reason() string {
	return strings.Join(strings.Fields(redact.Text(m.err.Error())), " ")
}

// names lists the sites that gave no answer: "site a" or "sites a, b".
func (s *search) names() string {
	var names []string
	for _, m := range s.missing {
		names = append(names, m.site)
	}
	if len(names) == 1 {
		return "site " + names[0]
	}
	return "sites " + strings.Join(names, ", ")
}

// incomplete is the error of a list of runs that left out a site, nil when
// every site answered.
func (s *search) incomplete() error {
	if len(s.missing) == 0 {
		return nil
	}
	return fmt.Errorf("the list of runs is incomplete: %s gave no answer, so the runs there are not listed", s.names())
}

// findRuns lists a workload's runs at every site that answers c, in the
// order of the Workload's sites. A site that gives no answer is left out,
// and named in the result; any other error of its check is returned, so a
// refused credential never hides a site's runs.
func findRuns(ctx context.Context, c kube.Clients, spec *failoverv1.WorkloadSpec, workload string) (*search, error) {
	// The checks of every site run at once ([RFC#0007] Requirement 10).
	defer engine.CheckAhead(ctx, c, spec)()
	out := &search{}
	for _, s := range spec.Sites {
		if err := c.Reachable(ctx, s.Name); err != nil {
			if kube.IsUnreachable(err) {
				out.missing = append(out.missing, missingSite{site: s.Name, err: err})
				continue
			}
			return nil, fmt.Errorf("site %s: search for runs: %w", s.Name, err)
		}
		cl, err := c.Client(s.Name)
		if err != nil {
			return nil, err
		}
		store := &ledger.Store{Client: cl, Namespace: spec.Ledger.Namespace, Selector: map[string]string{engine.LabelWorkload: workload}}
		items, err := store.List(ctx)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("site %s: list runs: %w", s.Name, err)
		}
		for i := range items {
			out.runs = append(out.runs, foundRun{site: s.Name, sf: &items[i]})
		}
	}
	return out, nil
}

// leftOut writes a progress line for each site that a search left out,
// once per site in this process ([RFC#0009] Requirement 16).
func (o *options) leftOut(w io.Writer, missing []missingSite) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, m := range missing {
		if o.named[m.site] {
			continue
		}
		if o.named == nil {
			o.named = map[string]bool{}
		}
		o.named[m.site] = true
		fmt.Fprintf(w, "  not read  the runs at site %s: %s\n", m.site, m.reason())
	}
}

// workloadRuns lists the workload's runs at every site of spec that
// answers c, as engine.Env.Runs does, and names on w each site it leaves
// out.
func (o *options) workloadRuns(spec *failoverv1.WorkloadSpec, workload string, w io.Writer) func(context.Context, kube.Clients) ([]failoverv1.SiteFailover, error) {
	return func(ctx context.Context, c kube.Clients) ([]failoverv1.SiteFailover, error) {
		found, err := findRuns(ctx, c, spec, workload)
		if err != nil {
			return nil, err
		}
		o.leftOut(w, found.missing)
		var runs []failoverv1.SiteFailover
		for _, r := range found.runs {
			runs = append(runs, *r.sf)
		}
		return runs, nil
	}
}

// pick returns the run named, or an error that names each site that gave
// no answer.
func (s *search) pick(name string) (foundRun, error) {
	for _, r := range s.runs {
		if r.sf.Name == name {
			return r, nil
		}
	}
	err := fmt.Sprintf("run %q not found at any reachable site", name)
	for _, m := range s.missing {
		err += fmt.Sprintf("; site %s gave no answer, so its runs were not read: %s", m.site, m.reason())
	}
	return foundRun{}, errors.New(err)
}

const resumeLong = `Resume continues a run from its ledger, never from memory: it takes the
remaining steps of the plan the run recorded when it started, with the
workload configuration it recorded then.

A step that was started but not recorded as done runs again; every step is
safe to repeat. Steps deferred because their site was unreachable run once
the site is back.

When the run stopped, the ledger's reason says what the operator must
decide. Each decision has its own flag:

  GitChanged           Git moved during the run. Review the new commits, then
                       --accept-git-head SHA makes that commit the new base.
                       When the stop names one commit, SHA must be that
                       commit; HEAD stands for it.
  FencedSiteIsPrimary  a site other than the new primary answers as
                       primary. --fence SITE lets this resume demote it,
                       when the message offers it. A resume without it
                       stops again while the site is primary.
  HeldByAnotherRun     another run holds an object. Finish that run, or
                       start a new run with --take-over.

A run that another run took over, or that another run of the workload
started after, cannot resume: its plan may no longer match the sites. Nor
can a run whose steps are not the plan its driver composes from the
ledger's Workload: the ledger was written outside tack.

An agent never starts "tack resume" without an explicit instruction from a
human for that run, and never passes these flags unless that instruction
names them.`

func newResume(o *options) *cobra.Command {
	var l ledgerFlags
	var yes bool
	var opts engine.Options
	cmd := &cobra.Command{
		Use:   "resume RUN (-f WORKLOAD | --context CONTEXT)",
		Short: "Continue a run from its ledger",
		Long:  resumeLong,
		Example: `  # Continue after Git is back (break-glass)
  tack resume pg-main-20261027-101500 -f workload.yaml

  # Accept a commit pushed during the run, after reviewing it
  tack resume pg-main-20261027-101500 -f workload.yaml --accept-git-head 1a2b3c4

  # Demote a lost site that came back as primary
  tack resume pg-main-20261027-101500 -f workload.yaml --fence a`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			progress := o.progress(out)
			found, err := l.find(ctx, o, args[0], progress)
			if err != nil {
				return err
			}
			r, err := found.pick(args[0])
			if err != nil {
				return err
			}
			spec := &r.sf.Spec.Workload.Spec
			ledgerSite := r.sf.Spec.To
			env, closeEnv, err := o.factory.Env(ctx, o.kubeconfig, spec, ledgerSite, progress)
			if err != nil {
				return err
			}
			defer closeEnv()
			env.Runs = o.workloadRuns(spec, r.sf.Spec.Workload.Name, progress)
			sf, err := env.Ledger.Get(ctx, r.sf.Name)
			if err != nil {
				return err
			}
			printStatus(out, sf)
			fmt.Fprintln(out)
			printClusters(out, sf, env.Clients)
			// Refuse a run that cannot resume, or a head the run did not
			// stop on, before asking the operator.
			res, err := env.CheckResume(ctx, sf)
			if err != nil {
				return err
			}
			head, err := engine.AcceptedHead(sf, opts.AcceptGitHead)
			if err != nil {
				return usageError{err}
			}
			if head == engine.AcceptHEAD {
				// The stop named no commit: the operator accepts the
				// branch head, and sees which commit before confirming.
				if head, err = env.AcceptHead(ctx, sf, head); err != nil {
					return err
				}
				fmt.Fprintf(out, "--accept-git-head HEAD is commit %s, the head of branch %s\n", head, spec.Git.Branch)
			}
			opts.AcceptGitHead, opts.Yes = head, yes
			if err := o.confirm(out, yes, sf.Spec.Workload.Name); err != nil {
				return err
			}
			return res.Resume(ctx, opts)
		},
	}
	l.add(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation; only when a human approved resuming this run")
	cmd.Flags().StringSliceVar(&opts.Fence, "fence", nil, "demote this site although it answers as primary (repeatable)")
	cmd.Flags().StringVar(&opts.AcceptGitHead, "accept-git-head", "", "accept this commit, pushed during the run, as the run's new Git base")
	return cmd
}

const statusLong = `Status shows runs from their ledgers. Without RUN it lists the runs of the
workload at every reachable site. A site that gives no answer is named after
the list, with its error, and status exits with code 1: the list is
incomplete. With -o yaml, the names follow the ledgers as YAML comments.

With RUN it shows that run's phases and steps with their times and
attempts, held objects as the ledger records them, Git commits, captured
values (by name only), follow-ups, and the history of the run's state: who
started, stopped and resumed it, when, with which tack and flags.`

func newStatus(o *options) *cobra.Command {
	var l ledgerFlags
	var output string
	cmd := &cobra.Command{
		Use:   "status [RUN] (-f WORKLOAD | --context CONTEXT)",
		Short: "Show runs and their steps from the ledger",
		Long:  statusLong,
		Example: `  # Every run of the workload
  tack status -f workload.yaml

  # One run, step by step
  tack status pg-main-20261027-101500 -f workload.yaml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "text" && output != "yaml" && output != "json" {
				return usagef("--output must be text, yaml or json")
			}
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			found, err := l.find(cmd.Context(), o, name, o.progress(cmd.ErrOrStderr()))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				// A site that gave no answer is named after the runs, and
				// fails the command: the list is incomplete ([RFC#0009]
				// Requirement 16).
				switch output {
				case "yaml":
					if err := writeYAML(out, found.runs); err != nil {
						return err
					}
					writeMissingYAML(out, found.missing)
					return found.incomplete()
				case "json":
					if err := writeJSONList(out, found.runs); err != nil {
						return err
					}
					// JSON has no comments: the names go to stderr.
					printMissing(cmd.ErrOrStderr(), found.missing)
					return found.incomplete()
				}
				printRuns(out, found.runs)
				printMissing(out, found.missing)
				return found.incomplete()
			}
			r, err := found.pick(args[0])
			if err != nil {
				return err
			}
			switch output {
			case "yaml":
				return writeYAML(out, []foundRun{r})
			case "json":
				return writeObject(out, "json", redacted(r.sf))
			}
			printStatus(out, r.sf)
			return nil
		},
	}
	l.add(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "text", "text, yaml or json")
	return cmd
}

// redacted returns a copy of a ledger whose Git URL and messages hold no
// URL's user or password, and name captured values. A ledger written
// before Tack refused or cleaned them may hold one (#35 row 43).
func redacted(sf *failoverv1.SiteFailover) *failoverv1.SiteFailover {
	out := sf.DeepCopy()
	g := &out.Spec.Workload.Spec.Git
	g.URL = redact.Text(g.URL)
	st := &out.Status
	st.Message = engine.Clean(st.Captured, st.Message)
	for i := range st.Steps {
		st.Steps[i].Message = engine.Clean(st.Captured, st.Steps[i].Message)
	}
	if st.Abandoned != nil {
		st.Abandoned.Reason = engine.Clean(st.Captured, st.Abandoned.Reason)
	}
	for i := range st.History {
		st.History[i].Message = engine.Clean(st.Captured, st.History[i].Message)
	}
	return out
}

// printMissing names, after the list of runs, each site that gave no
// answer, with its error.
func printMissing(w io.Writer, missing []missingSite) {
	if len(missing) == 0 {
		return
	}
	fmt.Fprintln(w, "\nSites that gave no answer; their runs are not listed:")
	for _, m := range missing {
		fmt.Fprintf(w, "  site %s: %s\n", m.site, m.reason())
	}
}

// writeMissingYAML names each site that gave no answer as a YAML comment
// after the ledgers, so the output still parses.
func writeMissingYAML(w io.Writer, missing []missingSite) {
	if len(missing) == 0 {
		return
	}
	fmt.Fprintln(w, "# Sites that gave no answer; their runs are not listed:")
	for _, m := range missing {
		fmt.Fprintf(w, "#   site %s: %s\n", m.site, m.reason())
	}
}

func writeYAML(w io.Writer, runs []foundRun) error {
	for i, r := range runs {
		if i > 0 {
			fmt.Fprintln(w, "---")
		}
		b, err := yaml.Marshal(redacted(r.sf))
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// writeObject writes one object as YAML or as indented JSON: the same
// content ([RFC#0009] Requirement 32).
func writeObject(w io.Writer, format string, obj any) error {
	var b []byte
	var err error
	if format == "json" {
		b, err = json.MarshalIndent(obj, "", "  ")
		b = append(b, '\n')
	} else {
		b, err = yaml.Marshal(obj)
	}
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// writeJSONList writes runs as one JSON List, as kubectl does: the YAML
// output's documents, in order, in its items.
func writeJSONList(w io.Writer, runs []foundRun) error {
	items := make([]*failoverv1.SiteFailover, 0, len(runs))
	for _, r := range runs {
		items = append(items, redacted(r.sf))
	}
	return writeObject(w, "json", map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}
