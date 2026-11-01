package engine

import (
	"math"
	"time"

	"github.com/coherera/quelor/pkg/resource"
)

// Event is emitted when a capacity machine crosses a timer or an AIMD/circuit transition.
type Event struct {
	Kind     string
	Resource string
	At       time.Time
	Token    string // wake grant token (Kind "wake")
}

type circuitMode int

const (
	modeClosed circuitMode = iota
	modeOpen
	modeHalfOpen
)

// resState is one resource's capacity machine. It is not safe for concurrent use;
// the Engine serializes callers.
type resState struct {
	id    string
	model resource.CapacityModel

	capacity int64
	minCap   int64
	window   time.Duration
	eff      int64

	inUse int64
	spent int64

	windowStart time.Time
	winID       uint64

	tokens     float64
	lastRefill time.Time

	circ          circuitMode
	consecFail    int
	failThreshold int
	openFor       time.Duration
	openUntil     time.Time
	probeInFlight bool
	probeDecided  bool

	blocked      bool
	blockedUntil time.Time
}

func newResState(m resource.Manifest, now time.Time) *resState {
	s := &resState{}
	s.id = m.ID
	s.model = m.CapacityModel
	s.capacity = m.Capacity
	if s.capacity < 0 {
		s.capacity = 0
	}
	s.minCap = m.MinCapacity
	if s.minCap < 0 {
		s.minCap = 0
	}
	s.window = time.Duration(m.Window)
	if s.window < 0 {
		s.window = 0
	}
	s.eff = s.capacity
	s.windowStart = now
	s.winID = 0
	s.tokens = float64(s.eff)
	s.lastRefill = now
	s.circ = modeClosed
	s.failThreshold = m.FailureThreshold
	s.openFor = time.Duration(m.OpenFor)
	if s.openFor < 0 {
		s.openFor = 0
	}
	return s
}

func (s *resState) effective() int64 { return s.eff }

func (s *resState) windowID() uint64 { return s.winID }

func (s *resState) avail(now time.Time) int64 {
	s.refill(now)
	if s.blocked && now.Before(s.blockedUntil) {
		return 0
	}
	return s.rawAvail()
}

func (s *resState) rawAvail() int64 {
	switch s.model {
	case resource.ModelLease:
		return nonNeg(s.eff - s.inUse)
	case resource.ModelHardBudget, resource.ModelWindowBucket:
		return nonNeg(s.eff - s.spent)
	case resource.ModelGCRA:
		a := int64(math.Floor(s.tokens))
		if a > s.eff {
			a = s.eff
		}
		return nonNeg(a)
	case resource.ModelCircuit:
		switch s.circ {
		case modeOpen:
			return 0
		case modeHalfOpen:
			if s.probeInFlight {
				return 0
			}
			return 1
		default:
			return math.MaxInt64 / 2
		}
	default:
		return 0
	}
}

func (s *resState) take(n int64, now time.Time) bool {
	if n <= 0 {
		return false
	}
	if s.avail(now) < n {
		return false
	}
	switch s.model {
	case resource.ModelLease:
		s.inUse += n
	case resource.ModelHardBudget, resource.ModelWindowBucket:
		s.spent += n
	case resource.ModelGCRA:
		s.tokens -= float64(n)
		if s.tokens < 0 {
			s.tokens = 0
		}
	case resource.ModelCircuit:
		if s.circ == modeHalfOpen {
			s.probeInFlight = true
			s.probeDecided = false
		}
	}
	return true
}

func (s *resState) addReservation(n int64, now time.Time) bool {
	return s.take(n, now)
}

// giveBack settles a reservation.
// Lease releases the full declared amount. Budget and window models refund
// max(0, declared-consumed), and a window refund applies only when window is the
// current window id. GCRA adds the refund back, capped at the effective capacity.
func (s *resState) giveBack(declared, consumed int64, window uint64, now time.Time) {
	if declared < 0 {
		declared = 0
	}
	if consumed < 0 {
		consumed = 0
	}
	s.refill(now)
	switch s.model {
	case resource.ModelLease:
		s.inUse -= declared
		if s.inUse < 0 {
			s.inUse = 0
		}
	case resource.ModelHardBudget:
		s.refundSpend(declared, consumed)
	case resource.ModelWindowBucket:
		if window != s.winID {
			return
		}
		s.refundSpend(declared, consumed)
	case resource.ModelGCRA:
		refund := declared - consumed
		if refund <= 0 {
			return
		}
		s.tokens += float64(refund)
		if s.tokens > float64(s.eff) {
			s.tokens = float64(s.eff)
		}
	}
}

func (s *resState) refundSpend(declared, consumed int64) {
	refund := declared - consumed
	if refund <= 0 {
		return
	}
	s.spent -= refund
	if s.spent < 0 {
		s.spent = 0
	}
}

// observe applies AIMD for lease, hard budget, window, and GCRA.
// Decrease is HTTP 429 or 503 only. Increase is 2xx/3xx and is emitted only when eff changes.
// Circuit breakers do not use AIMD; see observeCircuit.
func (s *resState) observe(status int, retryAfter time.Duration, now time.Time) []Event {
	if s.model == resource.ModelCircuit {
		return s.observeCircuit(status, now)
	}
	if status <= 0 {
		return nil
	}
	s.refill(now)
	switch {
	case status == 429 || status == 503:
		next := s.eff / 2
		if next < s.minCap {
			next = s.minCap
		}
		s.setEff(next)
		if retryAfter > 0 {
			s.blocked = true
			s.blockedUntil = now.Add(retryAfter)
		}
		return []Event{{Kind: "aimd-decrease", Resource: s.id, At: now}}
	case status >= 200 && status < 400:
		step := s.capacity / 20
		if step < 1 {
			step = 1
		}
		next := s.eff + step
		if next > s.capacity {
			next = s.capacity
		}
		if next == s.eff {
			return nil
		}
		s.setEff(next)
		return []Event{{Kind: "aimd-increase", Resource: s.id, At: now}}
	default:
		return nil
	}
}

func (s *resState) setEff(v int64) {
	if v < 0 {
		v = 0
	}
	s.eff = v
	if s.model == resource.ModelGCRA && s.tokens > float64(s.eff) {
		s.tokens = float64(s.eff)
	}
}

// Failures are status >= 500 or 429. Any other positive status is a success.
// Observations while the breaker is open do not close it; the half-open probe does.
func (s *resState) observeCircuit(status int, now time.Time) []Event {
	if status <= 0 {
		return nil
	}
	failure := status >= 500 || status == 429
	switch s.circ {
	case modeOpen:
		return nil
	case modeHalfOpen:
		s.probeDecided = true
		s.probeInFlight = false
		if failure {
			s.consecFail++
			return s.tripOpen(now)
		}
		return s.tripClosed(now)
	default:
		if failure {
			s.consecFail++
			if s.consecFail >= s.failThreshold {
				return s.tripOpen(now)
			}
			return nil
		}
		s.consecFail = 0
		return nil
	}
}

func (s *resState) tripOpen(now time.Time) []Event {
	s.circ = modeOpen
	s.openUntil = now.Add(s.openFor)
	return []Event{{Kind: "circuit-open", Resource: s.id, At: now}}
}

func (s *resState) tripClosed(now time.Time) []Event {
	s.circ = modeClosed
	s.consecFail = 0
	s.probeInFlight = false
	return []Event{{Kind: "circuit-closed", Resource: s.id, At: now}}
}

func (s *resState) probeSettled(ok bool, now time.Time) []Event {
	if s.model != resource.ModelCircuit {
		return nil
	}
	inFlight := s.probeInFlight
	decided := s.probeDecided
	s.probeInFlight = false
	s.probeDecided = false
	if !inFlight || decided || s.circ != modeHalfOpen {
		return nil
	}
	if ok {
		return s.tripClosed(now)
	}
	return s.tripOpen(now)
}

func (s *resState) advance(now time.Time) []Event {
	var ev []Event
	if s.model == resource.ModelWindowBucket && s.window > 0 {
		end := s.windowStart.Add(s.window)
		if !end.After(now) {
			steps := int64(now.Sub(s.windowStart) / s.window)
			base := s.windowStart
			for i := int64(0); i < steps; i++ {
				at := base.Add(time.Duration(i+1) * s.window)
				ev = append(ev, Event{Kind: "window-reset", Resource: s.id, At: at})
			}
			s.spent = 0
			s.winID += uint64(steps)
			s.windowStart = base.Add(time.Duration(steps) * s.window)
		}
	}
	if s.blocked && !s.blockedUntil.IsZero() && !now.Before(s.blockedUntil) {
		ev = append(ev, Event{Kind: "blocked-expired", Resource: s.id, At: now})
		s.blocked = false
	}
	if s.model == resource.ModelCircuit && s.circ == modeOpen && !s.openUntil.IsZero() && !now.Before(s.openUntil) {
		s.circ = modeHalfOpen
		s.probeInFlight = false
		s.probeDecided = false
		ev = append(ev, Event{Kind: "half-open", Resource: s.id, At: now})
	}
	return ev
}

func (s *resState) nextTimer() (time.Time, bool) {
	var next time.Time
	ok := false
	consider := func(t time.Time) {
		if !ok || t.Before(next) {
			next = t
			ok = true
		}
	}
	if s.blocked && !s.blockedUntil.IsZero() {
		consider(s.blockedUntil)
	}
	if s.model == resource.ModelWindowBucket && s.window > 0 {
		consider(s.windowStart.Add(s.window))
	}
	if s.model == resource.ModelCircuit && s.circ == modeOpen && !s.openUntil.IsZero() {
		consider(s.openUntil)
	}
	return next, ok
}

// wakeHint is the earliest time a new reservation of need is plausible.
// Already-possible admission returns now. Otherwise the hint is the earliest of
// an active retry-after, the next window boundary, a circuit half-open, or the
// GCRA refill time for the missing tokens. When none of those apply, it is now+1s.
func (s *resState) wakeHint(need int64, now time.Time) time.Time {
	if need <= 0 {
		return now
	}
	s.refill(now)
	blocked := s.blocked && now.Before(s.blockedUntil)
	if !blocked && s.rawAvail() >= need {
		return now
	}
	if s.model != resource.ModelCircuit && need > s.eff {
		return now.Add(time.Second)
	}
	fallback := now.Add(time.Second)
	switch s.model {
	case resource.ModelCircuit:
		if s.circ == modeOpen {
			if s.openUntil.After(now) {
				return s.openUntil
			}
			return now
		}
		return fallback
	case resource.ModelWindowBucket:
		if blocked && s.rawAvail() >= need {
			return s.blockedUntil
		}
		t := fallback
		if s.window > 0 {
			t = s.windowStart.Add(s.window)
		}
		if blocked && s.blockedUntil.After(t) {
			t = s.blockedUntil
		}
		if t.Before(now) {
			return now
		}
		return t
	case resource.ModelGCRA:
		t := now
		missing := float64(need) - s.tokens
		if missing > 0 {
			if s.window <= 0 || s.eff <= 0 {
				return fallback
			}
			rate := float64(s.eff) / s.window.Seconds()
			if rate <= 0 {
				return fallback
			}
			secs := missing / rate
			wait := time.Duration(math.Ceil(secs * float64(time.Second)))
			if wait < 0 {
				wait = 0
			}
			t = now.Add(wait)
		}
		if blocked && s.blockedUntil.After(t) {
			t = s.blockedUntil
		}
		if t.Before(now) {
			return now
		}
		return t
	default:
		if blocked {
			return s.blockedUntil
		}
		return fallback
	}
}

func (s *resState) refill(now time.Time) {
	if s.model != resource.ModelGCRA {
		return
	}
	if s.window <= 0 {
		if s.tokens > float64(s.eff) {
			s.tokens = float64(s.eff)
		}
		return
	}
	if !now.After(s.lastRefill) {
		return
	}
	elapsed := now.Sub(s.lastRefill).Seconds()
	if s.eff > 0 {
		rate := float64(s.eff) / s.window.Seconds()
		s.tokens += elapsed * rate
	}
	if s.tokens > float64(s.eff) {
		s.tokens = float64(s.eff)
	}
	if s.tokens < 0 {
		s.tokens = 0
	}
	s.lastRefill = now
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
