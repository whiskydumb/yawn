package queue

import "time"

type State string

const (
	StatePending     State = "pending"
	StateDownloading State = "downloading"
	StateDownloaded  State = "downloaded"
	StateUploading   State = "uploading"
	StateDone        State = "done"
	StateFailed      State = "failed"
	StateSkipped     State = "skipped"
)

type TargetState string

const (
	TargetPending   TargetState = "pending"
	TargetUploading TargetState = "uploading"
	TargetDone      TargetState = "done"
	TargetFailed    TargetState = "failed"
)

type SourceKind string

const (
	SourceChannel  SourceKind = "channel"
	SourcePlaylist SourceKind = "playlist"
	SourceVideo    SourceKind = "video"
	SourceList     SourceKind = "list"
)

type Source struct {
	ID       int64
	Kind     SourceKind
	URL      string
	Title    string
	AddedAt  time.Time
	SyncedAt *time.Time
}

type Item struct {
	ID       int64
	VideoID  string
	SourceID *int64
	URL      string

	Title      string
	Uploader   string
	UploadDate string
	Duration   int64

	State     State
	Attempts  int
	LastError string

	FormatID      string
	ExpectedBytes int64
	BytesDone     int64
	LocalPath     string
	LocalBytes    int64

	CreatedAt time.Time
	UpdatedAt time.Time
}

type Target struct {
	ID        int64
	ItemID    int64
	VideoID   string
	Store     string
	State     TargetState
	Attempts  int
	LastError string
	RemoteKey string
	Bytes     int64
	LocalPath string
	Item      Item
}

type Stats struct {
	ByState      map[State]int
	Total        int
	LocalBytes   int64
	TargetsTotal int
	TargetsDone  int
}

// Remaining counts videos that are not closed yet.
func (s Stats) Remaining() int {
	return s.Total - s.ByState[StateDone] - s.ByState[StateSkipped]
}
