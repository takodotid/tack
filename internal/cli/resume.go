package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/yaml"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
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
	sf   *v1alpha1.SiteFailover
}

// contextSite names the single site behind --context.
const contextSite = "context"

// find returns the runs the flags point at, newest first per site.
func (l *ledgerFlags) find(ctx context.Context, o *options) ([]foundRun, error) {
	switch {
	case l.file != "" && l.context != "":
		return nil, usagef("pass --file or --context, not both")
	case l.file != "":
		w, err := config.Load(l.file)
		if err != nil {
			return nil, err
		}
		return findRuns(ctx, o, &w.Spec, w.Name)
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
		var out []foundRun
		for i := range items {
			out = append(out, foundRun{site: "(" + l.context + ")", sf: &items[i]})
		}
		return out, nil
	}
	return nil, usagef("pass --file or --context")
}

// findRuns lists a workload's runs at every reachable site.
func findRuns(ctx context.Context, o *options, spec *v1alpha1.WorkloadSpec, workload string) ([]foundRun, error) {
	c := o.factory.Clients(o.kubeconfig, kube.Contexts(spec))
	var out []foundRun
	for _, s := range spec.Sites {
		if err := c.Reachable(ctx, s.Name); err != nil {
			continue
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
			out = append(out, foundRun{site: s.Name, sf: &items[i]})
		}
	}
	return out, nil
}

func pick(runs []foundRun, name string) (foundRun, error) {
	for _, r := range runs {
		if r.sf.Name == name {
			return r, nil
		}
	}
	return foundRun{}, fmt.Errorf("run %q not found at any reachable site", name)
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
  FencedSiteIsPrimary  a lost site came back and answers as primary.
                       --fence SITE lets the run demote it.
  HeldByAnotherRun     another run holds an object. Finish that run, or
                       start a new run with --take-over.

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
			runs, err := l.find(ctx, o)
			if err != nil {
				return err
			}
			r, err := pick(runs, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			spec := &r.sf.Spec.Workload.Spec
			ledgerSite := r.sf.Spec.To
			env, closeEnv, err := o.factory.Env(ctx, o.kubeconfig, spec, ledgerSite, out)
			if err != nil {
				return err
			}
			defer closeEnv()
			sf, err := env.Ledger.Get(ctx, r.sf.Name)
			if err != nil {
				return err
			}
			printStatus(out, sf)
			if err := o.confirm(out, yes, sf.Spec.Workload.Name); err != nil {
				return err
			}
			return env.Resume(ctx, sf, opts)
		},
	}
	l.add(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation; only when a human approved resuming this run")
	cmd.Flags().StringSliceVar(&opts.Fence, "fence", nil, "demote this site although it answers as primary (repeatable)")
	cmd.Flags().StringVar(&opts.AcceptGitHead, "accept-git-head", "", "accept this commit, pushed during the run, as the run's new Git base")
	return cmd
}

const statusLong = `Status shows runs from their ledgers. Without RUN it lists the runs of the
workload at every reachable site. With RUN it shows that run's phases, steps,
held objects, Git commits, captured values (by name only) and follow-ups.`

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
			if output != "text" && output != "yaml" {
				return usagef("--output must be text or yaml")
			}
			runs, err := l.find(cmd.Context(), o)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				if output == "yaml" {
					return writeYAML(out, runs)
				}
				printRuns(out, runs)
				return nil
			}
			r, err := pick(runs, args[0])
			if err != nil {
				return err
			}
			if output == "yaml" {
				return writeYAML(out, []foundRun{r})
			}
			printStatus(out, r.sf)
			return nil
		},
	}
	l.add(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "text", "text or yaml")
	return cmd
}

func writeYAML(w io.Writer, runs []foundRun) error {
	for i, r := range runs {
		if i > 0 {
			fmt.Fprintln(w, "---")
		}
		b, err := yaml.Marshal(r.sf)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}
