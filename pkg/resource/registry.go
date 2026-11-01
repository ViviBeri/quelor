package resource

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
)

// ErrNoReader is returned when a registered resource has no probe.
var ErrNoReader = errors.New("no reader registered")

// ErrNotFound is returned when the id was never registered.
var ErrNotFound = errors.New("resource not registered")

// ErrDuplicateID is returned when Register is called twice for one id.
var ErrDuplicateID = errors.New("duplicate resource id")

// Registry stores manifests and optional readers.
type Registry struct {
	mu      sync.RWMutex
	clock   func() time.Time
	mans    map[string]Manifest
	readers map[string]Reader
}

// NewRegistry returns an empty registry. A nil clock uses time.Now.
func NewRegistry(clock func() time.Time) *Registry {
	if clock == nil {
		clock = time.Now
	}
	return &Registry{
		clock:   clock,
		mans:    map[string]Manifest{},
		readers: map[string]Reader{},
	}
}

func (reg *Registry) now() time.Time {
	if reg.clock == nil {
		return time.Now()
	}
	return reg.clock()
}

// Register validates m, applies defaults, and stores it under m.ID.
// A nil reader means the resource is engine-local; Read then returns ErrNoReader.
// Registering the same id twice fails and leaves the first entry in place.
func (reg *Registry) Register(m Manifest, r Reader) error {
	m.WithDefaults()
	if err := m.Validate(); err != nil {
		return err
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.mans == nil {
		reg.mans = map[string]Manifest{}
		reg.readers = map[string]Reader{}
	}
	if _, exists := reg.mans[m.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateID, m.ID)
	}
	reg.mans[m.ID] = m
	reg.readers[m.ID] = r
	return nil
}

// Manifest returns a copy of the stored manifest.
func (reg *Registry) Manifest(id string) (Manifest, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	m, ok := reg.mans[id]
	return m, ok
}

// Has reports whether id is registered.
func (reg *Registry) Has(id string) bool {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	_, ok := reg.mans[id]
	return ok
}

// IDs returns registered ids in lexical order.
func (reg *Registry) IDs() []string {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	ids := make([]string, 0, len(reg.mans))
	for id := range reg.mans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Read calls the probe for id and applies freshness. AgeMS is now-ObservedAt
// in milliseconds. When FreshnessTTL is positive and age is strictly greater
// than the TTL, Stale is set; an age equal to the TTL stays fresh. A reader
// error or Known=false yields Known=false and a wrapped error for probe
// failures. Available is forced to 0 whenever the reading is not usable.
// A nil reader returns ErrNoReader.
func (reg *Registry) Read(ctx context.Context, id string) (quelor.Reading, error) {
	reg.mu.RLock()
	m, ok := reg.mans[id]
	rdr := reg.readers[id]
	reg.mu.RUnlock()
	if !ok {
		return quelor.Reading{Resource: id}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rdr == nil {
		return quelor.Reading{Resource: id}, fmt.Errorf("%w: %s", ErrNoReader, id)
	}
	reading, err := rdr.Read(ctx)
	reading.Resource = id
	if err != nil {
		reading.Known = false
	}
	reading = stampReading(reg.now(), m.FreshnessTTL, reading)
	if err != nil {
		return reading, fmt.Errorf("read %s: %w", id, err)
	}
	return reading, nil
}

// Available returns r.Available when the reading is usable capacity.
// Unknown and stale readings contribute 0.
func Available(r quelor.Reading) int64 {
	if !r.Usable() {
		return 0
	}
	return r.Available
}

// stampReading records age and marks the reading stale when a positive
// freshness TTL is strictly exceeded. An age equal to the TTL stays fresh.
// A probe that already set Stale keeps that mark. Available is forced to 0
// when the reading is not usable.
func stampReading(now time.Time, ttl Duration, in quelor.Reading) quelor.Reading {
	age := now.Sub(in.ObservedAt)
	in.AgeMS = age.Milliseconds()
	if ttl > 0 && age > time.Duration(ttl) {
		in.Stale = true
	}
	if !in.Usable() {
		in.Available = 0
	}
	return in
}
