package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/queue"
)

func newModel(t *testing.T, opts Options) *model {
	t.Helper()
	m := New(opts).(*model)
	m.width, m.height = 100, 30
	return m
}

func send(m *model, msgs ...tea.Msg) *model {
	for _, msg := range msgs {
		updated, _ := m.Update(msg)
		m = updated.(*model)
	}
	return m
}

func TestProgressCreatesOneRowPerItem(t *testing.T) {
	m := newModel(t, Options{})
	m = send(m,
		eventMsg(events.Event{Kind: events.KindDownloadStart, ItemID: 1, Title: "first", Worker: "dl-1"}),
		eventMsg(events.Event{Kind: events.KindProgress, ItemID: 1, Title: "first", Worker: "dl-1", Done: 50, Total: 100}),
		eventMsg(events.Event{Kind: events.KindProgress, ItemID: 1, Title: "first", Worker: "dl-1", Done: 75, Total: 100}),
	)
	if len(m.active) != 1 {
		t.Fatalf("got %d active rows, want 1", len(m.active))
	}
	if got := m.active[1].percent(); got != 0.75 {
		t.Errorf("percent = %v, want 0.75", got)
	}
}

func TestItemStaysOnOneRowThroughUpload(t *testing.T) {
	m := newModel(t, Options{})
	m = send(m,
		eventMsg(events.Event{Kind: events.KindDownloadStart, ItemID: 1, Title: "video", Worker: "dl-1"}),
		eventMsg(events.Event{Kind: events.KindProgress, ItemID: 1, Title: "video", Worker: "dl-1", Done: 100, Total: 100}),
		eventMsg(events.Event{Kind: events.KindUploadStart, ItemID: 1, Title: "video", Worker: "up-1", Store: "gdrive"}),
	)
	if len(m.order) != 1 {
		t.Fatalf("the video jumped to a new line on upload: %d rows", len(m.order))
	}
	if m.active[1].store != "gdrive" {
		t.Errorf("row did not switch to uploading: %+v", m.active[1])
	}
}

func TestDoneAndFailedRemoveTheRow(t *testing.T) {
	m := newModel(t, Options{})
	m = send(m,
		eventMsg(events.Event{Kind: events.KindDownloadStart, ItemID: 1, Title: "one"}),
		eventMsg(events.Event{Kind: events.KindDownloadStart, ItemID: 2, Title: "two"}),
		eventMsg(events.Event{Kind: events.KindItemDone, ItemID: 1, Title: "one"}),
		eventMsg(events.Event{Kind: events.KindFailed, ItemID: 2, Title: "two", Message: "gone"}),
	)
	if len(m.active) != 0 || len(m.order) != 0 {
		t.Errorf("rows survived completion: active=%d order=%d", len(m.active), len(m.order))
	}
	if len(m.log) != 2 {
		t.Errorf("got %d log lines, want 2", len(m.log))
	}
}

func TestLogIsBounded(t *testing.T) {
	m := newModel(t, Options{})
	for i := 0; i < maxLog*2; i++ {
		m = send(m, eventMsg(events.Event{Kind: events.KindInfo, Message: "line"}))
	}
	if len(m.log) > maxLog {
		t.Errorf("log grew to %d lines, the cap is %d", len(m.log), maxLog)
	}
}

func TestQuitStopsFirstAndExitsSecond(t *testing.T) {
	stopped := 0
	m := newModel(t, Options{Stop: func() { stopped++ }})

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	m = updated.(*model)
	if stopped != 1 {
		t.Fatalf("the first q called Stop %d times, want 1", stopped)
	}
	if cmd != nil {
		t.Error("the first q must not quit: the pipeline is still winding down")
	}
	if !m.stopping {
		t.Error("the model should show that it is stopping")
	}

	_, cmd = m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Error("the second q must quit right away")
	}
}

func TestFinishedClearsActiveRows(t *testing.T) {
	m := newModel(t, Options{})
	m = send(m,
		eventMsg(events.Event{Kind: events.KindDownloadStart, ItemID: 1, Title: "one"}),
		finishedMsg{},
	)
	if len(m.active) != 0 {
		t.Error("active rows must not linger after the pipeline finished")
	}
	if !m.finished {
		t.Error("the model should know it finished")
	}
}

func TestViewRendersWithoutStats(t *testing.T) {
	m := newModel(t, Options{Stores: []string{"gdrive"}, Quality: "1080"})
	out := m.View().Content
	if out == "" {
		t.Fatal("the view rendered empty")
	}
	if !strings.Contains(out, "yawn") {
		t.Error("the header is missing")
	}
	if !strings.Contains(out, "waiting for work") {
		t.Error("an idle pipeline should say so")
	}
}

func TestViewSurvivesATinyTerminal(t *testing.T) {
	m := newModel(t, Options{})
	m.width, m.height = 20, 5
	m = send(m,
		eventMsg(events.Event{Kind: events.KindProgress, ItemID: 1, Title: strings.Repeat("long title ", 20), Done: 1, Total: 3}),
		eventMsg(events.Event{Kind: events.KindInfo, Message: strings.Repeat("noise ", 40)}),
	)
	// the point is that it does not panic on a negative width
	if out := m.View().Content; out == "" {
		t.Error("the view rendered empty in a tiny terminal")
	}
}

func TestStatsComeFromTheQueue(t *testing.T) {
	want := queue.Stats{
		Total:   10,
		ByState: map[queue.State]int{queue.StateDone: 4, queue.StateFailed: 1},
	}
	m := newModel(t, Options{
		Stats: func(context.Context) (queue.Stats, error) { return want, nil },
	})
	cmd := m.pollStats()
	m = send(m, cmd())

	out := m.View().Content
	for _, fragment := range []string{"total", "10", "done", "4", "failed", "1", "40.0%"} {
		if !strings.Contains(out, fragment) {
			t.Errorf("the header is missing %q:\n%s", fragment, out)
		}
	}
}

func TestBar(t *testing.T) {
	if got := bar(0.5, 10); got != "#####....." {
		t.Errorf("bar(0.5, 10) = %q", got)
	}
	if got := bar(-1, 6); got != "......" {
		t.Errorf("an unknown size should render empty, got %q", got)
	}
	if got := bar(2, 4); got != "####" {
		t.Errorf("an overshoot should clamp, got %q", got)
	}
}
