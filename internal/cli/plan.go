package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"sigs.k8s.io/yaml"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
)

type planFlags struct {
	file   string
	from   string
	to     string
	mode   string
	output string
}

func (p *planFlags) add(cmd *cobra.Command, output bool) {
	cmd.Flags().StringVarP(&p.file, "file", "f", "", "the Workload file (required)")
	cmd.Flags().StringVar(&p.to, "to", "", "the site that becomes primary (required)")
	cmd.Flags().StringVar(&p.from, "from", "", "the site of the current primary (default: the primary the target site declares)")
	cmd.Flags().StringVar(&p.mode, "mode", "planned", "planned: the old primary demotes and hands over its token; site-loss: promote without a token after the old site is lost")
	if output {
		cmd.Flags().StringVarP(&p.output, "output", "o", "text", "text, or yaml for the SiteFailover the run would create")
	}
}

func (p *planFlags) request(w *config.Workload, now func() string) (engine.Request, error) {
	if p.file == "" || p.to == "" {
		return engine.Request{}, usagef("--file and --to are required")
	}
	var mode v1alpha1.Mode
	switch p.mode {
	case "planned":
		mode = v1alpha1.ModePlanned
	case "site-loss":
		mode = v1alpha1.ModeSiteLoss
	default:
		return engine.Request{}, usagef("--mode must be planned or site-loss, not %q", p.mode)
	}
	return engine.Request{
		Run:      w.Name + "-" + now(),
		Workload: w.Name,
		Spec:     &w.Spec,
		Request:  driver.Request{From: p.from, To: p.to, Mode: mode},
	}, nil
}

const planLong = `Plan prints every step and every change a run would make, in order, and
changes nothing. It reads each reachable site and the Git repository.

The plan names the objects Tack holds, the patches and waits of the
workload's driver, the exact Git change (file, path and value), the checks
before and after release, and the follow-ups left for the operator. Values
captured during the run, such as a demotion token, appear as $(name).

Run "tack plan" before every "tack run", and show its output to the human
who decides.`

func newPlan(o *options) *cobra.Command {
	var p planFlags
	cmd := &cobra.Command{
		Use:   "plan -f WORKLOAD --to SITE [--mode planned|site-loss]",
		Short: "Print every step and change of a run, and change nothing",
		Long:  planLong,
		Example: `  # A planned switchover to site b
  tack plan -f workload.yaml --to b

  # A failover after site a is lost
  tack plan -f workload.yaml --to b --mode site-loss

  # The SiteFailover object the run would create
  tack plan -f workload.yaml --to b -o yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if p.output != "text" && p.output != "yaml" {
				return usagef("--output must be text or yaml")
			}
			plan, _, cleanup, err := o.makePlan(cmd.Context(), &p, cmd.ErrOrStderr())
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if p.output == "yaml" {
				b, err := yaml.Marshal(plan.SiteFailover)
				if err != nil {
					return err
				}
				_, err = out.Write(b)
				return err
			}
			printPlan(out, plan)
			fmt.Fprintf(out, "\nNothing was changed. To run this plan: tack run -f %s --to %s --from %s --mode %s\n",
				p.file, plan.SiteFailover.Spec.To, plan.SiteFailover.Spec.From, p.mode)
			return nil
		},
	}
	p.add(cmd, true)
	return cmd
}

// makePlan loads the Workload, fills --from and composes the plan.
func (o *options) makePlan(ctx context.Context, p *planFlags, progress io.Writer) (*engine.Plan, *engine.Env, func(), error) {
	if p.file == "" || p.to == "" {
		return nil, nil, nil, usagef("--file and --to are required")
	}
	w, err := config.Load(p.file)
	if err != nil {
		return nil, nil, nil, err
	}
	req, err := p.request(w, func() string { return o.now().UTC().Format("20060102-150405") })
	if err != nil {
		return nil, nil, nil, err
	}
	env, closeEnv, err := o.factory.Env(ctx, o.kubeconfig, &w.Spec, p.to, progress)
	if err != nil {
		return nil, nil, nil, err
	}
	if req.From == "" {
		topo, err := env.Driver.Discover(ctx, &w.Spec, env.Clients)
		if err != nil {
			return nil, nil, closeEnv, err
		}
		to, _ := topo.Site(req.To)
		if to == nil || !to.Reachable {
			return nil, nil, closeEnv, fmt.Errorf("site %q must be reachable to become primary", req.To)
		}
		if to.DeclaredPrimary == "" || to.DeclaredPrimary == req.To {
			return nil, nil, closeEnv, usagef("site %s declares %q primary; pass --from", req.To, to.DeclaredPrimary)
		}
		req.From = to.DeclaredPrimary
		p.from = req.From
	}
	plan, err := env.MakePlan(ctx, req)
	return plan, env, closeEnv, err
}

const runLong = `Run starts a run: it prints the plan, asks for confirmation, creates the
SiteFailover ledger at the target site, and takes the steps in order.

Modes:
  planned    the old primary demotes; its demotion token promotes the new one
  site-loss  the new site promotes without a token; the old primary then
             needs a rebuild. Use it only when the old site is lost.

If Git cannot be reached (break-glass), the run still holds and switches the
live objects, then waits with exit code 4; "tack resume" finishes it once Git
is back. If anything unexpected happens, the run stops with exit code 3 and
the ledger says why.

An agent never starts "tack run" without an explicit instruction from a
human for that run, and runs "tack plan" first.`

func newRun(o *options) *cobra.Command {
	var p planFlags
	var yes bool
	var takeOver string
	cmd := &cobra.Command{
		Use:   "run -f WORKLOAD --to SITE [--mode planned|site-loss]",
		Short: "Start a run: hold Flux, switch the primary, commit to Git, release",
		Long:  runLong,
		Example: `  # A planned switchover to site b, after reviewing "tack plan"
  tack run -f workload.yaml --to b

  # A failover after site a is lost
  tack run -f workload.yaml --to b --mode site-loss`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			plan, env, cleanup, err := o.makePlan(cmd.Context(), &p, out)
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return err
			}
			printPlan(out, plan)
			if err := o.confirm(out, yes, plan.SiteFailover.Spec.Workload.Name); err != nil {
				return err
			}
			if err := checkNoActiveRun(cmd.Context(), o, env, plan.SiteFailover, takeOver); err != nil {
				return err
			}
			return env.Start(cmd.Context(), plan.SiteFailover, engine.Options{TakeOver: takeOver})
		},
	}
	p.add(cmd, false)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation; only when a human approved this run")
	cmd.Flags().StringVar(&takeOver, "take-over", "", "take over the holds of this earlier run, which can no longer finish")
	return cmd
}

// confirm asks the operator to type the workload's name.
func (o *options) confirm(out io.Writer, yes bool, workload string) error {
	if yes {
		return nil
	}
	if !o.isTerminal() {
		return usagef("no terminal to confirm on: pass --yes once a human approved this run")
	}
	fmt.Fprintf(out, "\nThis changes live clusters and Git. Type the workload name %q to go on: ", workload)
	line, err := bufio.NewReader(o.stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("no confirmation: %w", err)
	}
	if strings.TrimSpace(line) != workload {
		return fmt.Errorf("not confirmed; nothing was changed")
	}
	return nil
}

func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// checkNoActiveRun refuses a second active run of the same workload.
func checkNoActiveRun(ctx context.Context, o *options, env *engine.Env, sf *v1alpha1.SiteFailover, takeOver string) error {
	runs, err := findRuns(ctx, o, &sf.Spec.Workload.Spec, sf.Spec.Workload.Name)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.sf.Status.State == v1alpha1.RunSucceeded || r.sf.Name == takeOver {
			continue
		}
		return fmt.Errorf("run %s at site %s is %s: resume it (tack resume %s) or take it over (--take-over %s)",
			r.sf.Name, r.site, orPendingRun(r.sf.Status.State), r.sf.Name, r.sf.Name)
	}
	return nil
}
