package decider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const defaultShadowTimeout = 50 * time.Millisecond

// Slots is a Decider that returns Primary's decision unchanged.
// Each entry in Shadows is invoked concurrently and is strictly log-only:
// its response is passed to OnShadow and is never merged into the result.
// A shadow error or panic does not change Primary's result.
//
// ShadowTimeout defaults to 50ms. The shadow context keeps values from the
// caller context, drops the caller's cancellation and deadline, and adds its
// own timeout. Wait blocks until every shadow goroutine started so far has
// finished. An empty Shadows list starts no goroutines.
type Slots struct {
	Primary       Decider
	Shadows       []Decider
	ShadowTimeout time.Duration
	OnShadow      func(Request, Response, error)

	wg sync.WaitGroup
}

// Decide runs Primary and returns its result. Shadows are fan-out only.
func (s *Slots) Decide(ctx context.Context, req Request) (Response, error) {
	if s == nil {
		return Unavailable("", "nil slots"), fmt.Errorf("%w: nil slots", ErrUnavailable)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	shadows := s.Shadows
	onShadow := s.OnShadow
	timeout := effectiveShadowTimeout(s.ShadowTimeout)
	for _, sh := range shadows {
		if sh == nil {
			continue
		}
		s.wg.Add(1)
		go s.runShadow(ctx, req, sh, timeout, onShadow)
	}
	if s.Primary == nil {
		return Unavailable("", "no primary"), fmt.Errorf("%w: no primary", ErrUnavailable)
	}
	return s.Primary.Decide(ctx, req)
}

// Wait blocks until shadow goroutines started by Decide have returned.
func (s *Slots) Wait() {
	if s == nil {
		return
	}
	s.wg.Wait()
}

func (s *Slots) runShadow(parent context.Context, req Request, sh Decider, timeout time.Duration, onShadow func(Request, Response, error)) {
	defer s.wg.Done()
	defer func() {
		_ = recover()
	}()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()

	resp, err := decideRecover(sh, ctx, req)
	if onShadow != nil {
		onShadow(req, resp, err)
	}
}

func decideRecover(d Decider, ctx context.Context, req Request) (resp Response, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%w: shadow panic: %v", ErrUnavailable, rec)
			resp = Unavailable("", fmt.Sprintf("shadow panic: %v", rec))
		}
	}()
	return d.Decide(ctx, req)
}

func effectiveShadowTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultShadowTimeout
	}
	return d
}
