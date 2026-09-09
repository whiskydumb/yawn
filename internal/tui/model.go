package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/queue"
)

// StatsFunc reads the queue counters for the header.
type StatsFunc func(context.Context) (queue.Stats, error)

// DiskFunc reports the held and total disk budget.
type DiskFunc func() (used, capacity int64)

// Options are what the dashboard needs to render and to stop the run.
type Options struct {
	Events <-chan events.Event
	Stats  StatsFunc
	Disk   DiskFunc

	// Stop asks the pipeline to wind down. It must be safe to call twice.
	Stop func()

	Stores  []string
	Quality string
}

// row is one line in the active section.
type row struct {
	worker  string
	store   string
	title   string
	videoID string
	done    int64
	total   int64
	speed   float64
	updated time.Time
}

func (r row) label() string {
	if r.title != "" {
		return r.title
	}
	if r.videoID != "" {
		return r.videoID
	}
	return "(untitled)"
}

func (r row) percent() float64 {
	if r.total <= 0 {
		return -1
	}
	return float64(r.done) / float64(r.total)
}

type logLine struct {
	at   time.Time
	kind events.Kind
	text string
}

// maxLog is how many recent lines are kept. The dashboard is a live view,
// not a log reader: the full history goes to the log file.
const maxLog = 200

type model struct {
	opts Options

	width, height int
	active        map[int64]row
	order         []int64 // keeps the active rows in a stable position
	log           []logLine

	stats    queue.Stats
	started  time.Time
	stopping bool
	finished bool
	runErr   error
	quitting bool
}

// New builds the dashboard model.
func New(opts Options) tea.Model {
	return &model{
		opts:    opts,
		active:  make(map[int64]row),
		started: time.Now(),
		width:   80,
		height:  24,
	}
}

type (
	eventMsg    events.Event
	statsMsg    queue.Stats
	tickMsg     time.Time
	finishedMsg struct{ err error }
)

// Finished is the message the runner sends once the pipeline returns.
func Finished(err error) tea.Msg { return finishedMsg{err: err} }

// Event wraps a bus event as a bubbletea message.
func Event(e events.Event) tea.Msg { return eventMsg(e) }

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.pollStats(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) pollStats() tea.Cmd {
	return func() tea.Msg {
		if m.opts.Stats == nil {
			return nil
		}
		st, err := m.opts.Stats(context.Background())
		if err != nil {
			return nil
		}
		return statsMsg(st)
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case eventMsg:
		m.handleEvent(events.Event(msg))
		return m, nil

	case statsMsg:
		m.stats = queue.Stats(msg)
		return m, nil

	case tickMsg:
		// stats come from the queue rather than from counting events: a
		// dropped progress event must never make the totals drift
		return m, tea.Batch(tick(), m.pollStats())

	case finishedMsg:
		m.finished = true
		m.runErr = msg.err
		m.active = nil
		m.order = nil
		return m, nil
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		if m.finished || m.stopping {
			// already winding down, the second press means "right now"
			m.quitting = true
			return m, tea.Quit
		}
		m.stopping = true
		m.appendLog(events.KindWarn, "stopping, workers are finishing their current step")
		if m.opts.Stop != nil {
			m.opts.Stop()
		}
		return m, nil
	}
	return m, nil
}

func (m *model) handleEvent(e events.Event) {
	switch e.Kind {
	case events.KindProgress:
		m.upsert(e)

	case events.KindDownloadStart, events.KindUploadStart:
		m.upsert(e)

	case events.KindDownloadDone:
		m.appendLog(e.Kind, "downloaded  "+label(e))

	case events.KindUploadDone:
		m.appendLog(e.Kind, "uploaded    "+label(e)+" to "+e.Store)

	case events.KindItemDone:
		m.remove(e.ItemID)
		m.appendLog(e.Kind, "done        "+label(e))

	case events.KindFailed:
		m.remove(e.ItemID)
		m.appendLog(e.Kind, "failed      "+label(e)+": "+e.Message)

	case events.KindSkipped:
		m.remove(e.ItemID)
		m.appendLog(e.Kind, "skipped     "+label(e))

	case events.KindStalled, events.KindResumed, events.KindInfo, events.KindWarn:
		m.appendLog(e.Kind, e.Message)
	}
}

// upsert keeps one row per item, tracked by worker so a video moving from
// downloading to uploading stays on the same line instead of jumping.
func (m *model) upsert(e events.Event) {
	r, seen := m.active[e.ItemID]
	if !seen {
		m.order = append(m.order, e.ItemID)
	}
	r.worker = e.Worker
	r.store = e.Store
	r.videoID = e.VideoID
	if e.Title != "" {
		r.title = e.Title
	}
	if e.Kind == events.KindProgress {
		r.done, r.total, r.speed = e.Done, e.Total, e.Speed
	}
	if e.Kind == events.KindDownloadStart || e.Kind == events.KindUploadStart {
		r.done, r.total, r.speed = 0, 0, 0
	}
	r.updated = time.Now()
	m.active[e.ItemID] = r
}

func (m *model) remove(itemID int64) {
	delete(m.active, itemID)
	for i, id := range m.order {
		if id == itemID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *model) appendLog(kind events.Kind, text string) {
	m.log = append(m.log, logLine{at: time.Now(), kind: kind, text: text})
	if len(m.log) > maxLog {
		m.log = m.log[len(m.log)-maxLog:]
	}
}

func label(e events.Event) string {
	if e.Title != "" {
		return e.Title
	}
	if e.VideoID != "" {
		return e.VideoID
	}
	return "(untitled)"
}
