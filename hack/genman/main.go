// Command genman writes the man pages of tack into a directory.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/takodotid/tack/internal/cli"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: genman DIR")
		os.Exit(2)
	}
	// SOURCE_DATE_EPOCH makes the pages reproducible.
	date := time.Now().UTC()
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "SOURCE_DATE_EPOCH:", err)
			os.Exit(1)
		}
		date = time.Unix(n, 0).UTC()
	}
	if err := cli.WriteMan(os.Args[1], date); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
