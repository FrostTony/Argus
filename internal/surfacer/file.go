package surfacer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// File writes the measurement stream as line-delimited JSON, unaggregated.
type File struct {
	path string
	mu   sync.Mutex
	out  io.Writer
	c    io.Closer // nil for stdout and stderr, which are not ours to close
	w    *bufio.Writer
	enc  *json.Encoder
	// err is the first write error since Reopen or Close last reported one.
	err error
}

// NewFile accepts a path, or "stdout"/"stderr".
func NewFile(path string) (*File, error) {
	switch path {
	case "stdout", "-":
		return newFile(path, os.Stdout, nil), nil
	case "stderr":
		return newFile(path, os.Stderr, nil), nil
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return newFile(path, f, f), nil
}

func newFile(path string, out io.Writer, c io.Closer) *File {
	w := bufio.NewWriter(out)
	return &File{path: path, out: out, c: c, w: w, enc: json.NewEncoder(w)}
}

func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("surfacer file: %w", err)
	}
	return f, nil
}

// Reopen opens the path again, for logrotate, and reports write errors since
// the last report.
func (f *File) Reopen() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.c == nil {
		return f.takeErr()
	}
	// The new file is opened first: should the path be unusable for now (its
	// directory briefly gone), writing goes on to the old one until next time.
	file, err := openAppend(f.path)
	if err != nil {
		return errors.Join(f.takeErr(), err)
	}
	flushErr, closeErr := f.w.Flush(), f.c.Close()
	f.out, f.c = file, file
	f.w.Reset(file)
	return errors.Join(f.takeErr(), flushErr, closeErr)
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
			e.Value = number(float64(v))
		case metrics.Gauge:
			e.Value = number(float64(v))
		case metrics.Info:
			e.Value = string(v)
		case metrics.Observation:
			// In an event stream a measurement is just the number.
			e.Value = number(v.Value)
		case *metrics.Dist:
			e.Value = number(v.Sum)
		}
		f.keepErr(f.enc.Encode(e))
	}
	if err := f.w.Flush(); err != nil {
		f.keepErr(err)
		// bufio keeps a write error for good; starting clean lets a transient
		// one, such as a full disk, cost only this batch.
		f.w.Reset(f.out)
	}
}

// number is v for JSON, which has no literal for NaN and ±Inf: those become
// the strings Prometheus spells them with.
func number(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return v
}

func (f *File) keepErr(err error) {
	if f.err == nil && err != nil {
		f.err = fmt.Errorf("surfacer file: %w", err)
	}
}

func (f *File) takeErr() error {
	err := f.err
	f.err = nil
	return err
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := errors.Join(f.takeErr(), f.w.Flush())
	if f.c != nil {
		err = errors.Join(err, f.c.Close())
	}
	return err
}
