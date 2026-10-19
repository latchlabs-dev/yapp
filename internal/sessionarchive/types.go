package sessionarchive

import (
	"context"
	"errors"
	"time"

	"github.com/earlgrey/piproxy/internal/capture"
)

// API identifies the wire protocol of a captured request.
type API string

const (
	APIChat      API = "chat"
	APIResponses API = "responses"
	APIAnthropic API = "anthropic"
	APIUnknown   API = "unknown"
)

// TurnKind classifies how a captured request relates to the stored line.
type TurnKind string

const (
	// KindExtend: the request repeats the stored line and adds items after it.
	KindExtend TurnKind = "extend"
	// KindPrefix: the request is a strict prefix of the stored line (an
	// out-of-order capture or a replay), so it adds no input items.
	KindPrefix TurnKind = "prefix"
	// KindReset: the request diverges from the stored line (compaction, a
	// branch or a side request); items from the divergence point are stored.
	KindReset TurnKind = "reset"
	// KindAppend: a Responses request chained with previous_response_id, whose
	// input holds only the new items.
	KindAppend TurnKind = "append"
	// KindUnparsed: the request body could not be parsed (for example it was
	// truncated at capture); only its output is stored.
	KindUnparsed TurnKind = "unparsed"
)

const (
	segmentFormatVersion  = 1
	objectContentType     = "application/gzip"
	openingPromptMaxRunes = 16000
	promptPreviewMaxRunes = 500
	displayTextMaxRunes   = 32000
	errorTextMaxBytes     = 4096
	statusSuccess         = "success"
)

var (
	// ErrNoPayloads is returned by Rebuild when a conversation has no captured
	// requests to build from.
	ErrNoPayloads = errors.New("no captured requests for conversation")

	// ErrTranscriptNotFound is returned when a conversation has no transcript.
	ErrTranscriptNotFound = errors.New("session transcript not found")

	// ErrTranscriptRebuilding is returned when a transcript cannot be read
	// until a rebuild completes, for example because its objects were written
	// to a different object store than the one now configured.
	ErrTranscriptRebuilding = errors.New("session transcript is being rebuilt")

	errForeignObjectStore = errors.New("transcript objects are in a different object store")

	errRebuildRaced = errors.New("transcript generation changed during rebuild")
)

// State is the persisted builder view of one conversation's transcript: the
// display metadata shown without reading objects, plus what the builder needs
// to append the next request as a delta.
type State struct {
	TranscriptID       string
	TenantID           string
	UserID             string
	KeyID              string
	ConversationID     string
	Provider           string
	Model              string
	API                API
	OpeningPrompt      string
	LastPrompt         string
	RequestCount       int
	UserTurnCount      int
	ToolCallCount      int
	ErrorCount         int
	ResetCount         int
	StartedAt          time.Time
	LastActivityAt     time.Time
	Generation         int
	LineHashes         []byte
	SystemSHA          string
	LastSeq            int
	MergedThroughSeq   int
	MergedKey          string
	MergedBytes        int64
	SegmentBytes       int64
	LastOutputCount    int
	LastPayloadAt      *time.Time
	LastRequestID      string
	ObjectsLocation    string
	NeedsRebuild       bool
	RebuildRequestedAt *time.Time
	LastError          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// LineCount is the number of conversation items in the stored line.
func (s *State) LineCount() int { return len(s.LineHashes) / hashSize }

// TranscriptBytes is the compressed size of the current transcript objects.
func (s *State) TranscriptBytes() int64 { return s.MergedBytes + s.SegmentBytes }

// StoredIn reports whether the transcript's objects live in the object store
// at location. Keys are only meaningful in the store that wrote them: after a
// backend switch (postgres → gcs) an old transcript must be rebuilt, not read
// or appended to. Rows without a location (created by MarkNeedsRebuild) hold
// no objects yet, so any store matches.
func (s *State) StoredIn(location string) bool {
	return s.ObjectsLocation == "" || s.ObjectsLocation == location
}

// Turn is one captured request's entry in a transcript generation.
type Turn struct {
	Generation    int       `json:"generation"`
	Seq           int       `json:"seq"`
	RequestID     string    `json:"request_id"`
	CreatedAt     time.Time `json:"created_at"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	Status        string    `json:"status"`
	Kind          TurnKind  `json:"kind"`
	Base          int       `json:"base"`
	InputCount    int       `json:"input_count"`
	DeltaCount    int       `json:"delta_count"`
	OutputCount   int       `json:"output_count"`
	SystemChanged bool      `json:"system_changed"`
	PromptPreview string    `json:"prompt_preview,omitempty"`
	ToolNames     []string  `json:"tool_names"`
	SegmentKey    string    `json:"-"`
	SegmentBytes  int       `json:"segment_bytes"`
}

// StateTx is the view of the state store available while holding a
// conversation's lock. Everything written through it commits atomically when
// the lock callback returns nil.
type StateTx interface {
	// Load returns the conversation's state, or nil when none exists.
	Load(ctx context.Context) (*State, error)
	// Save upserts the transcript row and sets state.TranscriptID to the stored
	// row's id. With exact=false a needs_rebuild flag raised concurrently is
	// preserved; exact=true (a rebuild swap) writes the flag as given.
	Save(ctx context.Context, state *State, exact bool) error
	InsertTurns(ctx context.Context, transcriptID string, turns []Turn) error
	ListTurns(ctx context.Context, transcriptID string, generation int) ([]Turn, error)
	DeleteGeneration(ctx context.Context, transcriptID string, generation int) error
}

// StateStore persists transcript state and turns.
type StateStore interface {
	// WithConversationLock runs fn holding the conversation's exclusive lock
	// inside one transaction.
	WithConversationLock(ctx context.Context, tenantID, conversationID string, fn func(tx StateTx) error) error
	Get(ctx context.Context, tenantID, conversationID string) (*State, error)
	ListTurns(ctx context.Context, transcriptID string, generation int) ([]Turn, error)
	// MarkNeedsRebuild records that a capture could not be applied. It must not
	// require the conversation lock, because lock timeouts are one cause.
	MarkNeedsRebuild(ctx context.Context, p *capture.Payload, cause string) error
}

// PayloadRef identifies one captured request of a conversation.
type PayloadRef struct {
	RequestID string
	CreatedAt time.Time
}

// PayloadSource reads raw captures back for rebuilds.
type PayloadSource interface {
	// ListConversationRequests returns the conversation's captured requests
	// ordered by (created_at, request_id).
	ListConversationRequests(ctx context.Context, tenantID, conversationID string) ([]PayloadRef, error)
	// HasRequestsBefore reports whether the conversation has captures older
	// than before, other than excludeRequestID.
	HasRequestsBefore(ctx context.Context, tenantID, conversationID string, before time.Time, excludeRequestID string) (bool, error)
	// LoadPayload returns one capture with its bodies.
	LoadPayload(ctx context.Context, requestID string) (*capture.Payload, error)
}
