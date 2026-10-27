//go:build e2efault

package fault

import (
	"fmt"
	"os"
)

// Point exits the process when the interruption selects this step and stage.
func Point(step, stage string) {
	if os.Getenv(Env) == step+":"+stage {
		fmt.Fprintf(os.Stderr, "fault: interrupted at %s:%s\n", step, stage)
		os.Exit(ExitCode)
	}
}
