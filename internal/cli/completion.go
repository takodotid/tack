package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/takodotid/tack/internal/config"
)

const completionLong = `Completion prints a script that completes tack's commands, flags and
values in a shell: the values of --mode and --output, Workload files for
--file, and the site names of the Workload file given with --file for
--to, --from and --fence. Completion reads that file only: it never
contacts a cluster or Git.

Load it in the current shell, or install it as each shell's docs say:

  bash:        source <(tack completion bash)
  zsh:         source <(tack completion zsh)
  fish:        tack completion fish | source
  PowerShell:  tack completion powershell | Out-String | Invoke-Expression`

// newCompletion returns tack completion ([RFC#0009] Requirement 33).
func newCompletion() *cobra.Command {
	return &cobra.Command{
		Use:       "completion bash|zsh|fish|powershell",
		Short:     "Print a shell completion script, and contact nothing",
		Long:      completionLong,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root, out := cmd.Root(), cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			}
			return usagef("tack completion takes bash, zsh, fish or powershell, not %q", args[0])
		},
	}
}

// complete registers the completion of every command's flags and
// arguments. None contacts a cluster: site names come from the Workload
// file alone.
func complete(root *cobra.Command) {
	fixed := func(values ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return values, cobra.ShellCompDirectiveNoFileComp
		}
	}
	for _, cmd := range root.Commands() {
		if cmd.Name() == "completion" {
			continue
		}
		// RUN names live in ledgers at the sites: completing them would
		// contact a cluster.
		cmd.ValidArgsFunction = fixed()
		flags := cmd.Flags()
		reg := func(name string, f func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)) {
			if flags.Lookup(name) != nil {
				_ = cmd.RegisterFlagCompletionFunc(name, f)
			}
		}
		reg("file", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
		})
		reg("mode", fixed("planned", "site-loss"))
		reg("output", fixed("text", "yaml", "json"))
		for _, name := range []string{"to", "from", "fence"} {
			reg(name, sites)
		}
		for _, name := range []string{"context", "namespace", "take-over", "accept-git-head", "reason", "keep"} {
			reg(name, fixed())
		}
	}
}

// sites completes the site names of the Workload file that --file names.
// It reads the file only.
func sites(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	file, _ := cmd.Flags().GetString("file")
	if file == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	w, err := config.Load(file)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveError
	}
	var out []string
	for _, s := range w.Spec.Sites {
		out = append(out, fmt.Sprintf("%s\tcontext %s", s.Name, s.KubeContext))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
