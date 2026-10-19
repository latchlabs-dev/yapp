package main

import (
	"errors"
	"os"
	"strconv"

	"github.com/earlgrey/piproxy/internal/pdfextract"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	limit, err := strconv.Atoi(os.Args[1])
	if err == nil {
		err = pdfextract.RunWorker(os.Stdin, os.Stdout, limit)
	}
	switch {
	case err == nil:
		return
	case errors.Is(err, pdfextract.ErrOutputLimit):
		os.Exit(3)
	case errors.Is(err, pdfextract.ErrPageLimit):
		os.Exit(4)
	case errors.Is(err, pdfextract.ErrInputLimit):
		os.Exit(5)
	case errors.Is(err, pdfextract.ErrUnavailable):
		os.Exit(6)
	default:
		os.Exit(2)
	}
}
