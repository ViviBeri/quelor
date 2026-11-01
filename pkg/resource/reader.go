package resource

import (
	"context"
	"errors"
	"sync"

	"github.com/coherera/quelor/pkg/quelor"
)

// Reader returns the raw external reading for one resource.
// Available, Capacity, ObservedAt, and Known come from the probe.
type Reader interface {
	Read(ctx context.Context) (quelor.Reading, error)
}

// StaticReader is a fixed reading guarded by a mutex. It must not be copied.
type StaticReader struct {
	mu  sync.Mutex
	cur quelor.Reading
}

// NewStaticReader returns a reader that serves r until Set replaces it.
func NewStaticReader(r quelor.Reading) *StaticReader {
	return &StaticReader{cur: r}
}

// Set replaces the reading.
func (s *StaticReader) Set(r quelor.Reading) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = r
}

// Read returns a copy of the current reading.
func (s *StaticReader) Read(ctx context.Context) (quelor.Reading, error) {
	if s == nil {
		return quelor.Reading{}, errors.New("nil static reader")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, nil
}

// FuncReader adapts a function to Reader.
type FuncReader func(ctx context.Context) (quelor.Reading, error)

// Read calls the function.
func (f FuncReader) Read(ctx context.Context) (quelor.Reading, error) {
	if f == nil {
		return quelor.Reading{}, errors.New("nil func reader")
	}
	return f(ctx)
}

var (
	_ Reader = (*StaticReader)(nil)
	_ Reader = FuncReader(nil)
)
