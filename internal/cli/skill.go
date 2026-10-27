package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const skillDescription = "Plan and inspect switches of a stateful workload's primary between sites with the tack CLI, " +
	"and read SiteFailover ledgers. Use when asked about a Tack run, a site switchover or failover, or a SiteFailover. " +
	"Never start or resume a run without an explicit instruction from a human for that run."

// writeSkill writes the skill document: YAML frontmatter, then Markdown
// built from the command definitions, so it cannot drift from the help.
func writeSkill(w io.Writer, root *cobra.Command) error {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n\n", root.Name(), skillDescription)
	fmt.Fprintf(&b, "# %s\n\n%s.\n\n", root.Name(), root.Short)
	fmt.Fprintf(&b, "```text\n%s\n```\n\n", root.Long)
	b.WriteString("## Commands\n\n")
	for _, c := range root.Commands() {
		if !c.IsAvailableCommand() {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n%s.\n\n", c.CommandPath(), c.Short)
		fmt.Fprintf(&b, "```text\n%s\n```\n\n", c.Long)
		fmt.Fprintf(&b, "Usage: `%s`\n\n", c.UseLine())
		writeFlags(&b, c.LocalFlags())
		if c.Example != "" {
			fmt.Fprintf(&b, "Examples:\n\n```sh\n%s\n```\n\n", c.Example)
		}
	}
	b.WriteString("## Global flags\n\n")
	writeFlags(&b, root.PersistentFlags())
	_, err := io.WriteString(w, strings.TrimRight(b.String(), "\n")+"\n")
	return err
}

func writeFlags(b *strings.Builder, fs *pflag.FlagSet) {
	var rows []string
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		def := ""
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "[]" {
			def = " (default `" + f.DefValue + "`)"
		}
		rows = append(rows, fmt.Sprintf("| %s | %s%s |", name, strings.ReplaceAll(f.Usage, "|", "\\|"), def))
	})
	if len(rows) == 0 {
		return
	}
	b.WriteString("| Flag | Meaning |\n|---|---|\n")
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n\n")
}
