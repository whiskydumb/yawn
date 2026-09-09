package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/queue"
)

var (
	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleFail  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styleLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
)

func (m *model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}

	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	b.WriteString(m.activeSection())
	b.WriteString("\n")
	b.WriteString(m.logSection())
	b.WriteString("\n")
	b.WriteString(m.footer())
	return tea.NewView(b.String())
}

func (m *model) header() string {
	st := m.stats
	pct := 0.0
	if st.Total > 0 {
		pct = float64(st.ByState[queue.StateDone]) / float64(st.Total) * 100
	}

	title := styleTitle.Render("yawn")
	if m.opts.Quality != "" {
		title += styleDim.Render("  " + m.opts.Quality + "p")
	}
	if len(m.opts.Stores) > 0 {
		title += styleDim.Render("  -> " + strings.Join(m.opts.Stores, ", "))
	} else {
		title += styleDim.Render("  no stores, keeping files locally")
	}

	counts := fmt.Sprintf("%s %d   %s %d   %s %d   %s %d",
		styleLabel.Render("total"), st.Total,
		styleOK.Render("done"), st.ByState[queue.StateDone],
		styleLabel.Render("left"), st.Remaining(),
		styleFail.Render("failed"), st.ByState[queue.StateFailed])

	line := fmt.Sprintf("%s   %s", counts, styleDim.Render(fmt.Sprintf("%.1f%%", pct)))

	disk := ""
	if m.opts.Disk != nil {
		used, capacity := m.opts.Disk()
		if capacity > 0 {
			disk = fmt.Sprintf("%s %s %s / %s",
				styleLabel.Render("disk"),
				bar(float64(used)/float64(capacity), 16),
				humanBytes(used), humanBytes(capacity))
		}
	}

	out := title + "\n" + line
	if disk != "" {
		out += "\n" + disk
	}
	return out + "\n" + styleDim.Render(strings.Repeat("-", m.innerWidth()))
}

func (m *model) activeSection() string {
	if m.finished {
		return ""
	}
	if len(m.order) == 0 {
		return styleDim.Render("  waiting for work...")
	}

	var b strings.Builder
	for _, id := range m.order {
		r, ok := m.active[id]
		if !ok {
			continue
		}
		b.WriteString(m.renderRow(r))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *model) renderRow(r row) string {
	verb := "down"
	if r.store != "" {
		verb = "up"
	}
	worker := r.worker
	if worker == "" {
		worker = verb
	}

	pct := r.percent()
	progress := bar(pct, 18)
	pctText := "  ?  "
	if pct >= 0 {
		pctText = fmt.Sprintf("%3.0f%% ", pct*100)
	}

	speed := ""
	if r.speed > 0 {
		speed = humanBytes(int64(r.speed)) + "/s"
	}

	// the title gets whatever the fixed columns leave behind
	fixed := 8 + 18 + 6 + 12 + 4
	titleWidth := max(m.innerWidth()-fixed, 16)

	return fmt.Sprintf("  %-6s %s %s %s %s",
		styleDim.Render(worker),
		styleBar.Render(progress),
		pctText,
		padRight(trimRunes(r.label(), titleWidth), titleWidth),
		styleDim.Render(speed))
}

func (m *model) logSection() string {
	rows := m.logRows()
	if rows <= 0 || len(m.log) == 0 {
		return ""
	}
	start := max(len(m.log)-rows, 0)

	var b strings.Builder
	b.WriteString(styleDim.Render(strings.Repeat("-", m.innerWidth())))
	b.WriteString("\n")
	for _, l := range m.log[start:] {
		style := styleLabel
		switch l.kind {
		case events.KindFailed:
			style = styleFail
		case events.KindItemDone:
			style = styleOK
		case events.KindStalled, events.KindWarn:
			style = styleWarn
		case events.KindDownloadDone, events.KindUploadDone:
			style = styleDim
		}
		text := trimRunes(l.text, max(m.innerWidth()-11, 20))
		b.WriteString(fmt.Sprintf("  %s %s\n",
			styleDim.Render(l.at.Format("15:04:05")), style.Render(text)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// logRows gives the log whatever vertical space the other sections leave.
func (m *model) logRows() int {
	used := 5 + len(m.order) + 3
	if m.opts.Disk == nil {
		used--
	}
	return m.height - used
}

func (m *model) footer() string {
	elapsed := time.Since(m.started).Truncate(time.Second)

	var status string
	switch {
	case m.finished && m.runErr != nil:
		status = styleFail.Render("stopped: " + m.runErr.Error())
	case m.finished:
		status = styleOK.Render("finished")
	case m.stopping:
		status = styleWarn.Render("stopping, press q again to quit now")
	default:
		status = styleDim.Render("running " + elapsed.String())
	}

	keys := styleDim.Render("q stop and quit")
	if m.finished {
		keys = styleDim.Render("q quit")
	}
	return styleDim.Render(strings.Repeat("-", m.innerWidth())) + "\n  " + status + "   " + keys
}

func (m *model) innerWidth() int {
	return max(min(m.width, 120)-2, 40)
}

// bar draws a progress bar; a negative fraction means the size is unknown.
func bar(fraction float64, width int) string {
	if width < 4 {
		width = 4
	}
	if fraction < 0 {
		return strings.Repeat(".", width)
	}
	if fraction > 1 {
		fraction = 1
	}
	filled := int(fraction * float64(width))
	return strings.Repeat("#", filled) + strings.Repeat(".", width-filled)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func trimRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit <= 3 {
		return string(r[:limit])
	}
	return string(r[:limit-3]) + "..."
}

func padRight(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
