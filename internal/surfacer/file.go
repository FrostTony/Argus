package surfacer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// File writes the measurement stream as line-delimited JSON, unaggregated.
type File struct {
	path string
	mu   sync.Mutex
	w    *bufio.Writer
	c    io.Closer
	enc  *json.Encoder
}

// NewFile accepts a path, or "stdout"/"stderr".
func NewFile(path string) (*File, error) {
	var (
		w io.Writer
		c io.Closer
	)
	switch path {
	case "stdout", "-":
		w = os.Stdout
	case "stderr":
		w = os.Stderr
	default:
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("surfacer file: %w", err)
		}
		w, c = f, f
	}
	bw := bufio.NewWriter(w)
	return &File{path: path, w: bw, c: c, enc: json.NewEncoder(bw)}, nil
}

// Reopen closes the current file and opens the path again, for logrotate.
func (f *File) Reopen() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.c == nil {
		return nil // stdout/stderr: nothing to reopen
	}
	if err := f.w.Flush(); err != nil {
		return err
	}
	if err := f.c.Close(); err != nil {
		return err
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("surfacer file: %w", err)
	}
	f.c = file
	f.w = bufio.NewWriter(file)
	f.enc = json.NewEncoder(f.w)
	return nil
}

type event struct {
	Time   time.Time         `json:"time"`
	Name   string            `json:"name"`
	Kind   string            `json:"kind"`
	Labels map[string]string `json:"labels"`
	Value  any               `json:"value"`
}

func (f *File) Write(_ context.Context, b metrics.Batch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range b.Samples {
		e := event{Time: b.Time, Name: s.Name, Kind: s.Value.Kind().String(), Labels: s.Labels.Map()}
		switch v := s.Value.(type) {
		case metrics.Counter:
			e.Value = float64(v)
		case metrics.Gauge:
			e.Value = float64(v)
		case metrics.Info:
			e.Value = string(v)
		case metrics.Observation:
			// In an event stream a measurement is just the number.
			e.Value = v.Value
		case *metrics.Dist:
			e.Value = v.Sum
		}
		_ = f.enc.Encode(e)
	}
	_ = f.w.Flush()
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.w.Flush(); err != nil {
		return err
	}
	if f.c != nil {
		return f.c.Close()
	}
	return nil
}
