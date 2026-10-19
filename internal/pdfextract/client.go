// Package pdfextract keeps the eager PDF parser outside the proxy process.
package pdfextract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	MaxInputBytes  = 25 << 20
	MaxOutputBytes = 2 << 20
	MaxPages       = 128
	WorkerTimeout  = 3 * time.Second
)

var (
	ErrInputLimit  = errors.New("PDF exceeds extraction input limit")
	ErrOutputLimit = errors.New("PDF text exceeds extraction output limit")
	ErrPageLimit   = errors.New("PDF exceeds extraction page limit")
	ErrBusy        = errors.New("PDF extraction workers are busy")
	ErrUnavailable = errors.New("bounded PDF extraction worker is unavailable")
	ErrExtraction  = errors.New("PDF text extraction failed")
)

// Client bounds both concurrent workers and their wall-clock lifetime. Failed
// workers never fall back to running the parser in the caller's process.
type Client struct {
	slots   chan struct{}
	timeout time.Duration
	command func(context.Context, int) *exec.Cmd
}

func New(workerPath string) *Client {
	return &Client{
		slots: make(chan struct{}, 2), timeout: WorkerTimeout,
		command: func(ctx context.Context, limit int) *exec.Cmd {
			// workerPath is operator-selected or the sibling helper; the only argument
			// is a bounded integer. Request bytes are delivered exclusively on stdin.
			cmd := exec.CommandContext(ctx, workerPath, strconv.Itoa(limit)) // #nosec G204 -- trusted helper path, no shell or request-derived arguments.
			// The parser needs no provider credentials, storage keys, or inherited
			// application configuration. Limit its Go scheduler and soft heap too.
			cmd.Env = []string{"GOMAXPROCS=1", "GOMEMLIMIT=128MiB"}
			return cmd
		},
	}
}

var defaultClient = func() *Client {
	executable, err := os.Executable()
	if err != nil {
		return New("")
	}
	return New(filepath.Join(filepath.Dir(executable), "pdfextract"))
}()

// Extract uses the helper bundled beside the proxy binary.
func Extract(ctx context.Context, data []byte, outputLimit int64) (string, error) {
	return defaultClient.Extract(ctx, data, outputLimit)
}

func (c *Client) Extract(ctx context.Context, data []byte, outputLimit int64) (string, error) {
	if len(data) > MaxInputBytes {
		return "", ErrInputLimit
	}
	if outputLimit <= 0 {
		return "", ErrOutputLimit
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return "", ErrBusy
	}
	limit := int(min(outputLimit, MaxOutputBytes))
	workerCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := c.command(workerCtx, limit)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", ErrUnavailable
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return "", ErrUnavailable
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, int64(limit)+1))
	if len(output) > limit || readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if workerCtx.Err() != nil {
		return "", workerCtx.Err()
	}
	if len(output) > limit {
		return "", ErrOutputLimit
	}
	if readErr != nil {
		return "", ErrExtraction
	}
	if waitErr != nil {
		var exited *exec.ExitError
		if errors.As(waitErr, &exited) {
			switch exited.ExitCode() {
			case 3:
				return "", ErrOutputLimit
			case 4:
				return "", ErrPageLimit
			case 5:
				return "", ErrInputLimit
			case 6:
				return "", ErrUnavailable
			}
		}
		return "", ErrExtraction
	}
	return string(output), nil
}
