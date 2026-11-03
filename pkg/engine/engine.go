// Package engine is the deterministic Quelor admission engine: multi-resource
// leases, progressive renewal, AIMD, meta-admission, coalescing, tickets and
// timer events. No network or model calls happen on this path.
package engine

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/coherera/quelor/pkg/classify"
	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/decider/reference"
	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/resource"
	"github.com/coherera/quelor/pkg/trace"
)

var (
	ErrUnknownLease    = errors.New("unknown, settled or expired lease")
	ErrUnauthenticated = errors.New("holder not authenticated")
	ErrForbidden       = errors.New("holder does not own this lease")
)

// Quota caps what one holder may reserve. Zero / absent means unlimited.
type Quota struct {
	MaxLeases int              `json:"max_leases" yaml:"max_leases"`
	MaxUnits  map[string]int64 `json:"max_units" yaml:"max_units"`
}

// Config configures an Engine.
type Config struct {
	Registry       *resource.Registry
	Decider        decider.Decider // nil => reference.Default()
	Shadows        []decider.Decider
	Trace          trace.Writer // nil => trace.Nop
	Classifier     *classify.Ruleset
	Clock          func() time.Time
	LeaseTTL       time.Duration // default 30s
	TicketTTL      time.Duration // default 10s
	AgingStep      time.Duration // default 5s
	DeciderTimeout time.Duration // default 100ms
	SharedToken    string
	HolderTokens   map[string]string
	DefaultQuota   Quota
	HolderQuotas   map[string]Quota
	Cell           string
	Tracer         oteltrace.Tracer // nil => otel.Tracer("quelor")
}

// Stats are monotonic counters (ActiveLeases is a gauge).
type Stats struct {
	AcquireAdmit, AcquireDefer, AcquireShed, AcquireDeny  int64
	Renews, RenewRefused, Settles, DuplicateSettles       int64
	BoundUnreachable, AIMDDecreases, Expired, TraceErrors int64
	ActiveLeases                                          int64
}

// leaseRec is the engine's private record of an active lease.
type leaseRec struct {
	lease      quelor.Lease
	env        quelor.Envelope
	windows    map[string]uint64 // resource -> window id at reservation
	probe      map[string]bool   // circuit resources where this lease holds the half-open probe
	superseded bool
}

// ticket serialises contention on the scarcest resource.
type ticket struct {
	unitID  string
	need    int64
	rank    int
	since   time.Time
	expires time.Time
}

// waiter is a deferred unit awaiting a wake event.
type waiter struct {
	token    string
	resource string
	need     int64
	wakeAt   time.Time
}

// Engine implements quelor.LeaseAPI.
type Engine struct {
	cfg    Config
	slots  decider.Slots
	tracer oteltrace.Tracer

	mu       sync.Mutex
	res      map[string]*resState
	leases   map[string]*leaseRec  // by lease id
	byUnit   map[string]string     // unit id -> lease id
	byKey    map[string]string     // coalesce key -> lease id
	holders  map[string]*holderUse // holder -> usage
	tickets  map[string][]*ticket  // resource -> tickets
	waiters  map[string]*waiter    // token -> waiter
	settled  map[string]quelor.SettleResult
	settledQ []string // FIFO for eviction

	subMu sync.Mutex
	subs  []chan Event

	st stats
}

type holderUse struct {
	leases int
	units  map[string]int64
}

type stats struct {
	acquire                       [4]atomic.Int64 // admit, defer, shed, deny
	renews, renewRefused, settles atomic.Int64
	dupSettles, boundUnreachable  atomic.Int64
	aimdDec, expired, traceErrors atomic.Int64
}

var _ quelor.LeaseAPI = (*Engine)(nil)

// New builds an Engine. QUELOR_DECIDER=static forces the reference decider.
func New(cfg Config) (*Engine, error) {
	if cfg.Registry == nil {
		return nil, errors.New("engine: Registry is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.TicketTTL <= 0 {
		cfg.TicketTTL = 10 * time.Second
	}
	if cfg.AgingStep <= 0 {
		cfg.AgingStep = 5 * time.Second
	}
	if cfg.DeciderTimeout <= 0 {
		cfg.DeciderTimeout = 100 * time.Millisecond
	}
	if cfg.Trace == nil {
		cfg.Trace = trace.Nop
	}
	if cfg.Classifier == nil {
		cfg.Classifier = classify.Default()
	}
	primary := decider.Select(cfg.Decider, reference.Default())
	e := &Engine{
		cfg:     cfg,
		slots:   decider.Slots{Primary: primary, Shadows: cfg.Shadows},
		tracer:  cfg.Tracer,
		res:     map[string]*resState{},
		leases:  map[string]*leaseRec{},
		byUnit:  map[string]string{},
		byKey:   map[string]string{},
		holders: map[string]*holderUse{},
		tickets: map[string][]*ticket{},
		waiters: map[string]*waiter{},
		settled: map[string]quelor.SettleResult{},
	}
	if e.tracer == nil {
		e.tracer = otel.Tracer("quelor")
	}
	now := cfg.Clock()
	for _, id := range cfg.Registry.IDs() {
		m, _ := cfg.Registry.Manifest(id)
		e.res[id] = newResState(m, now)
	}
	return e, nil
}

func (e *Engine) now() time.Time { return e.cfg.Clock() }

// Close closes the trace writer.
func (e *Engine) Close() error { return e.cfg.Trace.Close() }

// Stats returns a snapshot of counters.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	active := int64(len(e.leases))
	e.mu.Unlock()
	return Stats{
		AcquireAdmit: e.st.acquire[0].Load(), AcquireDefer: e.st.acquire[1].Load(),
		AcquireShed: e.st.acquire[2].Load(), AcquireDeny: e.st.acquire[3].Load(),
		Renews: e.st.renews.Load(), RenewRefused: e.st.renewRefused.Load(),
		Settles: e.st.settles.Load(), DuplicateSettles: e.st.dupSettles.Load(),
		BoundUnreachable: e.st.boundUnreachable.Load(), AIMDDecreases: e.st.aimdDec.Load(),
		Expired: e.st.expired.Load(), TraceErrors: e.st.traceErrors.Load(), ActiveLeases: active,
	}
}

// countVerdict bumps the acquire counter for a verdict kind.
func (e *Engine) countVerdict(k quelor.VerdictKind) {
	switch k {
	case quelor.Admit:
		e.st.acquire[0].Add(1)
	case quelor.Defer:
		e.st.acquire[1].Add(1)
	case quelor.Shed:
		e.st.acquire[2].Add(1)
	case quelor.Deny:
		e.st.acquire[3].Add(1)
	}
}

// Authenticate reports whether token is valid for holder. With no tokens
// configured authentication is off and it returns true, as for the lease calls.
func (e *Engine) Authenticate(holder, token string) bool { return e.authenticate(holder, token) }

// authenticate checks a holder token. With no tokens configured auth is off.
func (e *Engine) authenticate(holder, token string) bool {
	if e.cfg.SharedToken == "" && len(e.cfg.HolderTokens) == 0 {
		return true
	}
	want := e.cfg.SharedToken
	if len(e.cfg.HolderTokens) > 0 {
		// Per-holder tokens are configured: only listed holders may act.
		t, ok := e.cfg.HolderTokens[holder]
		if !ok {
			return false
		}
		want = t
	}
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

func (e *Engine) quotaFor(holder string) Quota {
	if q, ok := e.cfg.HolderQuotas[holder]; ok {
		return q
	}
	return e.cfg.DefaultQuota
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// emit publishes events to subscribers without blocking.
func (e *Engine) emit(evs ...Event) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for _, ev := range evs {
		for _, ch := range e.subs {
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// Subscribe returns a buffered channel of engine events.
func (e *Engine) Subscribe() <-chan Event {
	ch := make(chan Event, 256)
	e.subMu.Lock()
	e.subs = append(e.subs, ch)
	e.subMu.Unlock()
	return ch
}

// writeTrace appends a record, counting failures instead of failing the call.
func (e *Engine) writeTrace(rec trace.Record) {
	if rec.TS == "" {
		rec.TS = e.now().UTC().Format(time.RFC3339Nano)
	}
	if rec.Cell == "" {
		rec.Cell = e.cfg.Cell
	}
	if err := e.cfg.Trace.Write(rec); err != nil {
		e.st.traceErrors.Add(1)
	}
}

// consult calls the primary decider (shadows log-only) with the configured
// timeout. It never holds e.mu. A non-nil error or non-answered status other
// than abstained means "unreachable" to the caller.
func (e *Engine) consult(ctx context.Context, req decider.Request) (decider.Response, error) {
	cctx, cancel := context.WithTimeout(ctx, e.cfg.DeciderTimeout)
	defer cancel()
	req.DeciderAPIVersion = decider.APIVersion
	type result struct {
		r   decider.Response
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := e.slots.Decide(cctx, req)
		ch <- result{r, err}
	}()
	select {
	case res := <-ch:
		return res.r, res.err
	case <-cctx.Done():
		return decider.Response{Status: decider.StatusUnavailable, DeciderID: "timeout"}, cctx.Err()
	}
}

// unreachable reports whether a consult result means the decider is unreachable.
func unreachable(r decider.Response, err error) bool {
	return err != nil || r.Status == decider.StatusUnavailable || r.Status == decider.StatusKilled
}
