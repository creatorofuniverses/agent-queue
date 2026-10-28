package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/units"
)

// ui is how human output is drawn. Plain output (no TTY, AQ_UI=plain) is the
// stable text agents read; pretty output is only for a person at a terminal.
type ui struct {
	pretty, color bool
	width         int
}

// detectUI picks pretty output for a terminal stdout. AQ_UI=pretty|plain forces
// the mode; NO_COLOR or TERM=dumb drop colors but keep the layout.
func detectUI(stdout io.Writer, env func(string) string) ui {
	u := ui{width: 100}
	if f, ok := stdout.(*os.File); ok {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
			u.pretty, u.width = true, int(ws.Col)
		}
	}
	switch env("AQ_UI") {
	case "pretty":
		u.pretty = true
	case "plain":
		u.pretty = false
	}
	u.color = u.pretty && env("NO_COLOR") == "" && env("TERM") != "dumb"
	return u
}

const (
	sgrBold   = "1"
	sgrDim    = "2"
	sgrRed    = "31"
	sgrGreen  = "32"
	sgrYellow = "33"
	sgrBlue   = "34"
	sgrCyan   = "36"
)

func (u ui) paint(code, s string) string {
	if !u.color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (u ui) bold(s string) string   { return u.paint(sgrBold, s) }
func (u ui) dim(s string) string    { return u.paint(sgrDim, s) }
func (u ui) red(s string) string    { return u.paint(sgrRed, s) }
func (u ui) green(s string) string  { return u.paint(sgrGreen, s) }
func (u ui) yellow(s string) string { return u.paint(sgrYellow, s) }
func (u ui) cyan(s string) string   { return u.paint(sgrCyan, s) }

// visibleLen is the printed width of s: escape sequences take no columns.
func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			esc = r != 'm'
		case r == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}

func pad(s string, w int) string {
	if d := w - visibleLen(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// truncate cuts plain text to w columns with an ellipsis.
func truncate(s string, w int) string {
	if w <= 1 || utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}

// paintState colors a bare state.
func (u ui) paintState(st model.State) string { return u.stateText(st, "") }

func stateReason(st model.State, reason model.Reason) string { return ui{}.stateText(st, reason) }

// stateText is STATE or STATE/REASON, colored by outcome.
func (u ui) stateText(st model.State, reason model.Reason) string {
	s := string(st)
	if reason != "" && !strings.EqualFold(string(reason), s) {
		s += "/" + string(reason)
	}
	switch st {
	case model.StateSucceeded, model.StateTornDown, model.StateReleased:
		return u.green(s)
	case model.StateRunning, model.StateGranted:
		return u.paint(sgrBlue, s)
	case model.StateQueued, model.StateStarting, model.StateCleaning, model.StateRecovering, model.StateExpiring, model.StateTearingDown:
		return u.yellow(s)
	case model.StateFailed, model.StateRejected, model.StateTeardownFailed, model.StateTeardownUnknown:
		return u.red(s)
	case model.StateCancelled:
		return u.dim(s)
	}
	return s
}

// stateMark is a one-cell status glyph for headings.
func (u ui) stateMark(st model.State) string {
	switch {
	case st == model.StateSucceeded || st == model.StateTornDown || st == model.StateReleased:
		return u.green("✓")
	case st == model.StateFailed || st == model.StateRejected || st == model.StateTeardownFailed || st == model.StateTeardownUnknown:
		return u.red("✗")
	case st == model.StateCancelled:
		return u.dim("–")
	case st == model.StateRunning || st == model.StateGranted:
		return u.paint(sgrBlue, "●")
	}
	return u.yellow("○")
}

// bar draws used/total as a fixed-width meter; past warn it turns yellow, past
// crit red.
func (u ui) bar(used, total uint64, width int) string {
	if width < 4 {
		width = 4
	}
	frac := 0.0
	if total > 0 {
		frac = min(float64(used)/float64(total), 1)
	}
	n := int(frac*float64(width) + 0.5)
	full := strings.Repeat("█", n)
	switch {
	case frac >= 0.9:
		full = u.red(full)
	case frac >= 0.7:
		full = u.yellow(full)
	default:
		full = u.green(full)
	}
	return full + u.dim(strings.Repeat("░", width-n))
}

// table prints aligned columns; cells may carry colors.
type table struct {
	head  []string
	rows  [][]string
	right map[int]bool // right-aligned columns
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) write(u ui, w io.Writer) {
	widths := make([]int, len(t.head))
	for i, h := range t.head {
		widths[i] = visibleLen(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			widths[i] = max(widths[i], visibleLen(c))
		}
	}
	line := func(cells []string, style func(string) string) {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			if style != nil {
				c = style(c)
			}
			if t.right[i] {
				b.WriteString(strings.Repeat(" ", max(0, widths[i]-visibleLen(c))) + c)
			} else if i < len(cells)-1 {
				b.WriteString(pad(c, widths[i]))
			} else {
				b.WriteString(c)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(t.head, func(s string) string { return u.bold(s) })
	for _, r := range t.rows {
		line(r, nil)
	}
}

// heading is a bold section title.
func (u ui) heading(w io.Writer, s string) { fmt.Fprintln(w, u.bold(s)) }

// field prints an aligned "label  value" line of a card.
func (u ui) field(w io.Writer, label, value string) {
	fmt.Fprintf(w, "  %s %s\n", u.dim(pad(label, 12)), value)
}

// human durations: 0s, 850ms, 42s, 3m05s, 2h10m, 3d4h.
func fmtDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		d = d.Round(time.Second)
		if int(d.Seconds())%60 == 0 {
			return fmt.Sprintf("%dm", int(d.Minutes()))
		}
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		d = d.Round(time.Minute)
		if int(d.Minutes())%60 == 0 {
			return fmt.Sprintf("%dh", int(d.Hours()))
		}
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	d = d.Round(time.Hour)
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func fmtSecs(s float64) string { return fmtDur(time.Duration(s * float64(time.Second))) }

func size(b uint64) string { return units.FormatSize(b) }

// shortProfile drops the home directory from a profile key for display and
// keeps at most 60 columns, cutting the start of the path.
func shortProfile(p, home string) string {
	if home != "" && strings.HasPrefix(p, home+"/") {
		p = "~" + p[len(home):]
	}
	if r := []rune(p); len(r) > 60 {
		p = "…" + string(r[len(r)-59:])
	}
	return p
}

// startIn is the expected start for people: now, in 5m, or unknown.
func startIn(at *time.Time) string {
	if at == nil {
		return "unknown"
	}
	if d := time.Until(*at); d > 0 {
		return "in " + fmtDur(d)
	}
	return "now"
}
