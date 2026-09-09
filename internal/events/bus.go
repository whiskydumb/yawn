package events

import (
	"sync"
	"time"
)

type Kind string

const (
	KindQueued        Kind = "queued"
	KindDownloadStart Kind = "download_start"
	KindProgress      Kind = "progress"
	KindDownloadDone  Kind = "download_done"
	KindUploadStart   Kind = "upload_start"
	KindUploadDone    Kind = "upload_done"
	KindItemDone      Kind = "item_done"
	KindFailed        Kind = "failed"
	KindSkipped       Kind = "skipped"
	KindInfo          Kind = "info"
	KindWarn          Kind = "warn"
	KindStalled       Kind = "stalled"
	KindResumed       Kind = "resumed"
)

type Event struct {
	Kind    Kind
	Time    time.Time
	ItemID  int64
	VideoID string
	Title   string
	Worker  string
	Store   string
	Message string

	// progress fields, filled only for KindProgress
	Done  int64
	Total int64
	Speed float64
	ETA   int64
}

func (e Event) Percent() float64 {
	if e.Total <= 0 {
		return -1
	}
	return float64(e.Done) / float64(e.Total)
}

// Bus fans events out to subscribers. Publishing never blocks: a slow
// subscriber drops events rather than stalling a download, which is the right
// trade for progress updates since a fresher one follows immediately.
type Bus struct {
	mu     sync.RWMutex
	subs   map[int]chan Event
	nextID int
	buffer int
	closed bool
}

func NewBus(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 256
	}
	return &Bus{subs: make(map[int]chan Event), buffer: buffer}
}

// Subscribe returns an event channel and the unsubscribe function the
// subscriber must call.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, b.buffer)
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = ch

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber is behind, drop on purpose
		}
	}
}

func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}
