package pdfextract

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func compressedPDF(t *testing.T, text string, pages int) []byte {
	t.Helper()
	var compressed bytes.Buffer
	zip := zlib.NewWriter(&compressed)
	_, _ = fmt.Fprintf(zip, "BT /F1 12 Tf (%s) Tj ET", text)
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [3 0 R] /Count %d >>", pages),
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", compressed.Len(), compressed.String()),
	}
	var file bytes.Buffer
	file.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, file.Len())
		fmt.Fprintf(&file, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := file.Len()
	fmt.Fprintf(&file, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&file, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&file, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return file.Bytes()
}

func TestWorkerCompressedPDFExpansionAndLimits(t *testing.T) {
	workerPath := os.Getenv("PIPROXY_PDF_EXTRACTOR_TEST_BINARY")
	if workerPath == "" {
		t.Skip("PIPROXY_PDF_EXTRACTOR_TEST_BINARY is not set")
	}
	client := New(workerPath)
	data := compressedPDF(t, strings.Repeat("A", 65536), 1)
	if len(data) >= 1024 {
		t.Fatalf("fixture did not compress enough: %d bytes", len(data))
	}
	if text, err := client.Extract(context.Background(), data, 1024); !errors.Is(err, ErrOutputLimit) || text != "" {
		t.Fatalf("compressed expansion was not rejected: bytes=%d err=%v", len(text), err)
	}
	text, err := client.Extract(context.Background(), data, 128<<10)
	if err != nil || !strings.Contains(text, strings.Repeat("A", 65536)) {
		t.Fatalf("bounded PDF text layer stopped working: bytes=%d err=%v", len(text), err)
	}
	if _, err := client.Extract(context.Background(), compressedPDF(t, "text", MaxPages+1), 1024); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("page cap error = %v", err)
	}
	if text, err := client.Extract(context.Background(), compressedPDF(t, strings.Repeat("A", MaxOutputBytes+128), 1), MaxOutputBytes*2); !errors.Is(err, ErrOutputLimit) || text != "" {
		t.Fatalf("fixed 2 MiB output cap was bypassed: bytes=%d err=%v", len(text), err)
	}
}

func TestWorkerHardLimitHelper(t *testing.T) {
	mode := os.Getenv("PIPROXY_PDF_HARD_LIMIT_TEST")
	if mode == "" {
		return
	}
	if err := applyLimits(); err != nil {
		os.Exit(6)
	}
	if mode == "memory" {
		data := make([]byte, 512<<20)
		for i := range data {
			data[i] = byte(i)
		}
		runtime.KeepAlive(data)
	} else {
		var counter atomic.Uint64
		for {
			counter.Add(1)
		}
	}
	os.Exit(0)
}

func TestWorkerLinuxHardMemoryAndCPULimits(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hard worker limits are Linux-only; other platforms fail closed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"memory", "cpu"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestWorkerHardLimitHelper$")
			cmd.Env = []string{"PIPROXY_PDF_HARD_LIMIT_TEST=" + mode, "GOMAXPROCS=1"}
			err := cmd.Run()
			if ctx.Err() != nil || err == nil {
				t.Fatalf("hard %s limit did not terminate the worker: err=%v context=%v", mode, err, ctx.Err())
			}
			var exited *exec.ExitError
			if errors.As(err, &exited) && exited.ExitCode() == 6 {
				t.Fatal("worker could not install its hard limits")
			}
		})
	}
}

func TestWorkerUnsupportedKernelFailsClosed(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux provides the required worker limits")
	}
	var output bytes.Buffer
	err := RunWorker(bytes.NewReader([]byte("%PDF-1.4")), &output, 1024)
	if !errors.Is(err, ErrUnavailable) || output.Len() != 0 {
		t.Fatalf("unsupported kernel parsed input: output=%d err=%v", output.Len(), err)
	}
}
