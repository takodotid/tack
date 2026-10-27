package cli

import (
	"os"
	"time"

	"github.com/spf13/cobra/doc"
)

// WriteMan writes tack(1) and one page per subcommand into dir.
func WriteMan(dir string, date time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root := NewRoot(nil)
	root.DisableAutoGenTag = true
	header := &doc.GenManHeader{Title: "TACK", Section: "1", Date: &date, Source: "tack " + Version, Manual: "Tack Manual"}
	return doc.GenManTree(root, header, dir)
}
