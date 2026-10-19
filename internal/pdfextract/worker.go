package pdfextract

import (
	"bytes"
	"io"

	"github.com/ledongthuc/pdf"
)

// RunWorker is for the dedicated helper only. The PDF library eagerly expands
// streams and builds page text, so its CPU/memory limits must be installed
// before even opening the input. The parent separately caps output and kills
// the whole worker on timeout; a parser panic/OOM cannot affect the proxy.
func RunWorker(input io.Reader, output io.Writer, outputLimit int) (err error) {
	if err := applyLimits(); err != nil {
		return ErrUnavailable
	}
	if outputLimit <= 0 || outputLimit > MaxOutputBytes {
		return ErrOutputLimit
	}
	defer func() {
		if recover() != nil {
			err = ErrExtraction
		}
	}()
	data, err := io.ReadAll(io.LimitReader(input, MaxInputBytes+1))
	if err != nil {
		return ErrExtraction
	}
	if len(data) > MaxInputBytes {
		return ErrInputLimit
	}
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ErrExtraction
	}
	pages := reader.NumPage()
	if pages < 0 || pages > MaxPages {
		return ErrPageLimit
	}
	remaining := outputLimit
	for page := 1; page <= pages; page++ {
		text, err := reader.Page(page).GetPlainText(nil)
		if err != nil {
			return ErrExtraction
		}
		if len(text) > remaining {
			return ErrOutputLimit
		}
		if _, err := io.WriteString(output, text); err != nil {
			return ErrExtraction
		}
		remaining -= len(text)
	}
	return nil
}
