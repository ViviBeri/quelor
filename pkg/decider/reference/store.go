package reference

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coherera/quelor/pkg/decider"
)

// Store is a content-addressed ruleset registry. Put never deletes.
// Hashes are returned in first-insert order. The first object stored for a
// hash is kept. Rulesets are treated as immutable after Parse.
type Store struct {
	mu      sync.RWMutex
	byHash  map[string]*Ruleset
	hashes  []string
	active  *publication
	history []publication
}

type publication struct {
	hash string
	mode decider.Mode
}

type storeDecider struct {
	s *Store
}

var _ decider.Decider = storeDecider{}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{byHash: make(map[string]*Ruleset)}
}

// Put stores rs under its content hash. A second put of the same hash is a no-op.
func (s *Store) Put(rs *Ruleset) string {
	if rs == nil {
		return ""
	}
	hash := rs.Hash()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byHash[hash]; ok {
		return hash
	}
	s.byHash[hash] = rs
	s.hashes = append(s.hashes, hash)
	return hash
}

// Get returns the ruleset stored for hash.
func (s *Store) Get(hash string) (*Ruleset, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rs, ok := s.byHash[hash]
	return rs, ok
}

// Activate publishes hash and repoints the active ruleset.
// The previous publication is pushed on the history stack.
// mode must be log-only, advise, or enforce and overrides YAML mode.
func (s *Store) Activate(hash string, mode decider.Mode) error {
	switch mode {
	case decider.ModeLogOnly, decider.ModeAdvise, decider.ModeEnforce:
	default:
		return fmt.Errorf("reference: invalid mode %q", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byHash[hash]; !ok {
		return fmt.Errorf("reference: unknown ruleset %q", hash)
	}
	if s.active != nil {
		s.history = append(s.history, *s.active)
	}
	s.active = &publication{hash: hash, mode: mode}
	return nil
}

// Active returns the active ruleset and the mode from Activate.
func (s *Store) Active() (*Ruleset, decider.Mode) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil, ""
	}
	rs := s.byHash[s.active.hash]
	if rs == nil {
		return nil, ""
	}
	return rs, s.active.mode
}

// Revert repoints the active ruleset at the previous publication.
// The current object stays in the store. An empty history returns an error
// and leaves the current publication in place.
func (s *Store) Revert() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return errors.New("reference: no previous ruleset")
	}
	prev := s.history[len(s.history)-1]
	s.history = s.history[:len(s.history)-1]
	s.active = &publication{hash: prev.hash, mode: prev.mode}
	return nil
}

// Hashes returns a copy of stored hashes in insertion order.
func (s *Store) Hashes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.hashes))
	copy(out, s.hashes)
	return out
}

// Decider evaluates whichever ruleset is active at call time.
// The mode passed to Activate overrides the YAML mode.
func (s *Store) Decider() decider.Decider {
	return storeDecider{s: s}
}

func (d storeDecider) Decide(ctx context.Context, req decider.Request) (decider.Response, error) {
	rs, mode := d.s.Active()
	return evaluate(ctx, rs, mode, req)
}
