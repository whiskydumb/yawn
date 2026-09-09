package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/queue"
)

// sampleFrame builds a dashboard in a realistic state.
func sampleFrame(t *testing.T) *model {
	t.Helper()
	st := queue.Stats{
		Total: 1556,
		ByState: map[queue.State]int{
			queue.StateDone: 505, queue.StateFailed: 3, queue.StatePending: 1048,
		},
	}
	m := newModel(t, Options{
		Stats:   func(context.Context) (queue.Stats, error) { return st, nil },
		Disk:    func() (int64, int64) { return 27 << 30, 40 << 30 },
		Stores:  []string{"gdrive"},
		Quality: "1080",
	})
	m.width, m.height = 96, 24
	m = send(m, m.pollStats()())
	return send(m,
		eventMsg(events.Event{Kind: events.KindProgress, ItemID: 1, Worker: "dl-1",
			Title: "Hunger games #17", Done: 74 << 20, Total: 120 << 20, Speed: 4.4e6}),
		eventMsg(events.Event{Kind: events.KindProgress, ItemID: 2, Worker: "dl-2",
			Title: "LP: CrazyCraft v2.0 #1", Done: 9 << 20, Total: 210 << 20, Speed: 1.1e6}),
		eventMsg(events.Event{Kind: events.KindProgress, ItemID: 3, Worker: "up-1", Store: "gdrive",
			Title: "Karty ot podpischikov #9", Done: 180 << 20, Total: 200 << 20}),
		eventMsg(events.Event{Kind: events.KindItemDone, ItemID: 4, Title: "Hunger games #16"}),
		eventMsg(events.Event{Kind: events.KindFailed, ItemID: 5, Title: "Some video", Message: "video unavailable"}),
	)
}

func TestFrameFitsTheTerminal(t *testing.T) {
	m := sampleFrame(t)
	out := m.View().Content

	lines := strings.Split(out, "\n")
	if len(lines) > m.height {
		t.Errorf("the frame is %d lines tall, the terminal is %d", len(lines), m.height)
	}
	for i, l := range lines {
		if w := visibleWidth(l); w > m.width {
			t.Errorf("line %d is %d columns wide, the terminal is %d:\n%s", i, w, m.width, l)
		}
	}
}

func TestFrameShowsEveryWorkerAndTheLog(t *testing.T) {
	out := stripANSI(sampleFrame(t).View().Content)
	for _, fragment := range []string{
		"dl-1", "dl-2", "up-1",
		"Hunger games #17", "Karty ot podpischikov #9",
		"done        Hunger games #16",
		"failed      Some video: video unavailable",
		"disk", "27.0 GiB / 40.0 GiB",
	} {
		if !strings.Contains(out, fragment) {
			t.Errorf("the frame is missing %q:\n%s", fragment, out)
		}
	}
	if os.Getenv("YAWN_TUI_FRAME") != "" {
		t.Log("\n" + sampleFrame(t).View().Content)
	}
}

// visibleWidth counts printable columns, ignoring the escape codes lipgloss
// wraps the styles in.
func visibleWidth(s string) int {
	return len([]rune(stripANSI(s)))
}

func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape && (r == 'm' || r == 'K' || r == 'J' || r == 'H'):
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
}
