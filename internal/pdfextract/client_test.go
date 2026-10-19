package pdfextract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPDFWorkerProcessHelper(t *testing.T) {
	if os.Getenv("PIPROXY_PDF_TEST_HELPER") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "overflow":
		_, _ = fmt.Print(strings.Repeat("x", 128<<10))
	case "timeout":
		time.Sleep(10 * time.Second)
	case "success":
		_, _ = fmt.Print("bounded extracted text")
	}
	os.Exit(0)
}

func helperClient(t *testing.T, mode string) *Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := New("")
	client.command = func(ctx context.Context, _ int) *exec.Cmd {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPDFWorkerProcessHelper$", "--", mode)
		cmd.Env = append(os.Environ(), "PIPROXY_PDF_TEST_HELPER=1")
		return cmd
	}
	return client
}

func TestExtractBoundsWorkerOutput(t *testing.T) {
	client := helperClient(t, "overflow")
	text, err := client.Extract(context.Background(), []byte("pdf input"), 1024)
	if !errors.Is(err, ErrOutputLimit) || text != "" {
		t.Fatalf("overflow returned text=%d bytes err=%v", len(text), err)
	}
}

func TestExtractKillsTimedOutWorker(t *testing.T) {
	client := helperClient(t, "timeout")
	client.timeout = 100 * time.Millisecond
	start := time.Now()
	text, err := client.Extract(context.Background(), []byte("pdf input"), 1024)
	if !errors.Is(err, context.DeadlineExceeded) || text != "" {
		t.Fatalf("timeout returned text=%q err=%v", text, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("timed-out worker was not reaped promptly")
	}
}

func TestExtractBoundsConcurrencyAndReleasesSlots(t *testing.T) {
	client := helperClient(t, "timeout")
	client.timeout = 200 * time.Millisecond
	started := make(chan struct{}, 2)
	original := client.command
	client.command = func(ctx context.Context, limit int) *exec.Cmd {
		started <- struct{}{}
		return original(ctx, limit)
	}
	finished := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := client.Extract(context.Background(), []byte("pdf"), 1024); finished <- err }()
	}
	<-started
	<-started
	if _, err := client.Extract(context.Background(), []byte("pdf"), 1024); !errors.Is(err, ErrBusy) {
		t.Fatalf("full worker pool error = %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("worker did not exit after timeout: %v", err)
		}
	}
	if len(client.slots) != 0 {
		t.Fatalf("timed-out workers retained %d slots", len(client.slots))
	}
	client.command = helperClient(t, "success").command
	client.timeout = WorkerTimeout
	text, err := client.Extract(context.Background(), []byte("pdf"), 1024)
	if err != nil || text != "bounded extracted text" {
		t.Fatalf("worker slot was not reusable: text=%q err=%v", text, err)
	}
}

func TestExtractRefusesOversizedInputBeforeSpawning(t *testing.T) {
	client := New("/not/a/worker")
	if _, err := client.Extract(context.Background(), make([]byte, MaxInputBytes+1), 1024); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("oversized input error = %v", err)
	}
}

func TestExtractMissingWorkerFailsClosed(t *testing.T) {
	client := New("/not/a/pdf-extraction-worker")
	text, err := client.Extract(context.Background(), []byte("%PDF-1.4"), 1024)
	if !errors.Is(err, ErrUnavailable) || text != "" || len(client.slots) != 0 {
		t.Fatalf("missing worker did not fail closed: text=%q err=%v slots=%d", text, err, len(client.slots))
	}
}
