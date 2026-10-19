package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JSONLFileSink appends metadata events to a host-persisted JSON Lines file.
// The envelope intentionally matches the former HTTP hook receiver so existing
// logs and consumers remain compatible across the in-process cutover.
type JSONLFileSink struct {
	path string
	file *os.File

	mu     sync.Mutex
	closed bool
}

type fileRecord struct {
	ReceivedAt string            `json:"received_at"`
	LatencyMS  float64           `json:"latency_ms"`
	Headers    map[string]string `json:"headers"`
	Event      json.RawMessage   `json:"event"`
}

func NewJSONLFileSink(path string) (*JSONLFileSink, error) {
	if path == "" {
		return nil, errors.New("hook events file is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create hook events directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open hook events file: %w", err)
	}
	return &JSONLFileSink{path: path, file: file}, nil
}

func (s *JSONLFileSink) Name() string { return "jsonl-file" }

func (s *JSONLFileSink) Write(ctx context.Context, ev *Event) error {
	if s == nil || ev == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	started := time.Now()
	normalize(ev)
	// This sink is a metadata event log. It predates content-aware hooks and
	// never opted in, so prompt content is stripped rather than written to disk
	// on the proxy host.
	metadataOnly := *ev
	metadataOnly.Content = nil
	body, err := json.Marshal(&metadataOnly)
	if err != nil {
		return fmt.Errorf("marshal hook event: %w", err)
	}
	entry, err := json.Marshal(fileRecord{
		ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		LatencyMS:  float64(time.Since(started).Nanoseconds()) / float64(time.Millisecond),
		Headers: map[string]string{
			"X-Piproxy-Event-ID":   ev.ID,
			"X-Piproxy-Event-Type": string(ev.Type),
			"X-Piproxy-Timestamp":  ev.EmittedAt.UTC().Format(time.RFC3339Nano),
		},
		Event: body,
	})
	if err != nil {
		return fmt.Errorf("marshal hook event record: %w", err)
	}
	entry = append(entry, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("hook events file is closed")
	}
	if err := writeAll(s.file, entry); err != nil {
		return fmt.Errorf("append hook event to %s: %w", s.path, err)
	}
	// Make the append durable before acknowledging it to the manager. This is
	// off the request hot path and preserves the old sink's restart durability
	// while strengthening its host-crash behavior.
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sync hook events file %s: %w", s.path, err)
	}
	return nil
}

func (s *JSONLFileSink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.file.Sync(); err != nil {
		_ = s.file.Close()
		return fmt.Errorf("sync hook events file %s: %w", s.path, err)
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("close hook events file %s: %w", s.path, err)
	}
	return nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
