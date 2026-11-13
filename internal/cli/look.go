package cli

import (
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// look is how output looks on its stream ([RFC#0009] Requirement 31):
// symbols and tables with lines on a terminal, colours too unless NO_COLOR
// is set; on anything else, such as a pipe or a file, plain text, the same
// bytes as before.
type look struct {
	// term is set when the stream is a terminal.
	term bool
	// color is set on a terminal without NO_COLOR.
	color bool
	// width is the terminal's width, 0 when unknown.
	width int
}

// terminal is a writer that says whether it is a terminal, and how wide;
// tests use it.
type terminal interface {
	IsTerminal() bool
	Width() int
}

// lookOf returns how output to w looks.
func lookOf(w io.Writer) look {
	var l look
	switch t := w.(type) {
	case terminal:
		l.term, l.width = t.IsTerminal(), t.Width()
	case *os.File:
		if fd := int(t.Fd()); term.IsTerminal(fd) {
			l.term = true
			l.width, _, _ = term.GetSize(fd)
		}
	}
	l.color = l.term && os.Getenv("NO_COLOR") == ""
	return l
}

// ANSI styles. Each colour has one meaning: red stops, yellow warns, green
// is done, dim is detail, bold is a heading.
const (
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiReset  = "\x1b[0m"
)

func (l look) paint(style, s string) string {
	if !l.color || s == "" {
		return s
	}
	return style + s + ansiReset
}

func (l look) bold(s string) string   { return l.paint(ansiBold, s) }
func (l look) dim(s string) string    { return l.paint(ansiDim, s) }
func (l look) red(s string) string    { return l.paint(ansiRed, s) }
func (l look) green(s string) string  { return l.paint(ansiGreen, s) }
func (l look) yellow(s string) string { return l.paint(ansiYellow, s) }

// state colours a run's or a step's state word: green when done, yellow
// while it waits or goes on, red when it stopped or failed, dim when it
// never goes on.
func (l look) state(s string) string {
	switch s {
	case "Succeeded":
		return l.green(s)
	case "Stopped", "Failed":
		return l.red(s)
	case "Running", "Waiting", "Deferred":
		return l.yellow(s)
	case "Abandoned", "TakenOver", "Pending":
		return l.dim(s)
	}
	return s
}

// note colours a plan's note: a warning in yellow.
func (l look) note(s string) string {
	if strings.HasPrefix(s, "warning: ") {
		return l.yellow(s)
	}
	return s
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visible is how wide text is on screen: colours take no room.
func visible(s string) int { return utf8.RuneCountInString(ansi.ReplaceAllString(s, "")) }

// box writes rows in a table with thin lines and bold headings, as on a
// terminal. Columns in right line up on the right. The last column wraps
// when the table is wider than the terminal.
func (l look) box(w io.Writer, indent string, head []string, right map[int]bool, rows [][]string) {
	widths := make([]int, len(head))
	for i, h := range head {
		widths[i] = visible(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], visible(c))
		}
	}
	last := len(head) - 1
	if l.width > 0 {
		room := l.width - len(indent) - 1 - 3*len(head)
		others := 0
		for _, n := range widths[:last] {
			others += n
		}
		if others+widths[last] > room {
			widths[last] = max(visible(head[last]), room-others, 12)
		}
	}
	line := func(left, mid, rt string) string {
		parts := make([]string, len(widths))
		for i, n := range widths {
			parts[i] = strings.Repeat("─", n+2)
		}
		return indent + l.dim(left+strings.Join(parts, mid)+rt) + "\n"
	}
	bar := l.dim("│")
	cell := func(s string, i int) string {
		pad := strings.Repeat(" ", max(0, widths[i]-visible(s)))
		if right[i] {
			return pad + s
		}
		return s + pad
	}
	row := func(cells []string) string {
		wrapped := wrap(cells[last], widths[last])
		var b strings.Builder
		for n, part := range wrapped {
			b.WriteString(indent + bar)
			for i := range cells {
				c := part
				if i < last {
					c = ""
					if n == 0 {
						c = cells[i]
					}
				}
				b.WriteString(" " + cell(c, i) + " " + bar)
			}
			b.WriteString("\n")
		}
		return b.String()
	}
	io.WriteString(w, line("┌", "┬", "┐"))
	bold := make([]string, len(head))
	for i, h := range head {
		bold[i] = l.bold(h)
	}
	io.WriteString(w, row(bold))
	io.WriteString(w, line("├", "┼", "┤"))
	for _, r := range rows {
		io.WriteString(w, row(r))
	}
	io.WriteString(w, line("└", "┴", "┘"))
}

// wrap splits text into lines of at most width characters, at spaces; a
// longer word is cut. A text that wraps loses its colours.
func wrap(s string, width int) []string {
	if visible(s) <= width {
		return []string{s}
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(ansi.ReplaceAllString(s, "")) {
		next := word
		if line != "" {
			next = line + " " + word
		}
		if utf8.RuneCountInString(next) <= width {
			line = next
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
		for r := []rune(word); ; r = r[width:] {
			if len(r) <= width {
				line = string(r)
				break
			}
			lines = append(lines, string(r[:width]))
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// progressLine colours one progress line, its time first: the time dim,
// and its word by what it means, with a symbol for a step done.
func (l look) progressLine(stamp, rest string) string {
	if !l.term {
		return stamp + rest
	}
	out := l.dim(stamp)
	word := strings.TrimSpace(strings.SplitN(strings.TrimLeft(rest, " "), "  ", 2)[0])
	switch {
	case strings.HasPrefix(rest, "  done "):
		return out + l.green("✔ done") + strings.TrimPrefix(rest, "  done")
	case word == "warning" || word == "deferred" || word == "retry" || word == "note" || word == "not read":
		i := strings.Index(rest, word)
		return out + rest[:i] + l.yellow(word) + rest[i+len(word):]
	case strings.HasPrefix(rest, "run ") && strings.Contains(rest, " succeeded"):
		return out + l.green(rest)
	}
	return out + rest
}
