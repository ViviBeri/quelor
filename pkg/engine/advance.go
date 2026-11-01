package engine

import (
	"context"
	"sort"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/trace"
)

// runPoll caps timer sleeps. Engine has no wake field, so a timer armed during
// a sleep is noticed on the next poll.
const runPoll = 250 * time.Millisecond

// Advance applies every timer due at now: resource timers, lease expiry, ticket
// expiry, then waiter wakes. It returns the events it applied and is a no-op
// when nothing is due.
func (e *Engine) Advance(now time.Time) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()

	var evs []Event
	for _, id := range sortedMapKeys(e.res) {
		evs = append(evs, e.res[id].advance(now)...)
	}
	evs = append(evs, e.expireLeasesLocked(now)...)
	evs = append(evs, e.expireTicketsLocked(now)...)
	evs = append(evs, e.wakeWaitersLocked(now)...)
	e.emit(evs...)
	return evs
}

// expireLeasesLocked reclaims every active lease with ExpiresAt <= now.
// A nil consumed map means the reservation was unused (consumed == declared).
func (e *Engine) expireLeasesLocked(now time.Time) []Event {
	recs := make([]*leaseRec, 0, len(e.leases))
	for _, rec := range e.leases {
		if rec == nil || rec.lease.ExpiresAt.After(now) {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool {
		return recs[i].lease.ID < recs[j].lease.ID
	})

	evs := make([]Event, 0, len(recs))
	for _, rec := range recs {
		leaseID := rec.lease.ID
		decisionRef := rec.lease.DecisionRef
		holderID := rec.lease.HolderID
		tenantID := rec.lease.TenantID
		env := rec.env
		env.Demands = copyAmounts(rec.env.Demands)
		declaredSnap := copyAmounts(rec.lease.Declared)

		res := e.releaseLocked(rec, nil, now, "expire")
		e.storeSettledLocked(res)
		e.st.expired.Add(1)

		declared := copyAmounts(res.Declared)
		if declared == nil {
			declared = declaredSnap
		}
		consumed := copyAmounts(res.Consumed)
		if consumed == nil {
			consumed = copyAmounts(declared)
		}
		e.writeTrace(trace.Record{
			TS:            now.UTC().Format(time.RFC3339Nano),
			Op:            trace.OpExpire,
			DecisionRef:   decisionRef,
			EnvelopeHash:  trace.HashEnvelope(env),
			Envelope:      &env,
			HolderID:      holderID,
			TenantID:      tenantID,
			LeaseID:       leaseID,
			Verdict:       &quelor.Verdict{Stamps: []string{"lease-expired"}},
			DeciderStatus: trace.DeciderStatusNone,
			Stamps:        []string{"lease-expired"},
			Declared:      declared,
			Consumed:      consumed,
		})
		evs = append(evs, Event{
			Kind:     "lease-expired",
			Resource: "",
			At:       now,
			Token:    leaseID,
		})
	}
	return evs
}

func (e *Engine) expireTicketsLocked(now time.Time) []Event {
	var evs []Event
	for _, id := range sortedMapKeys(e.tickets) {
		src := e.tickets[id]
		kept := make([]*ticket, 0, len(src))
		for _, tk := range src {
			if tk == nil || !tk.expires.After(now) {
				if tk == nil {
					continue
				}
				evs = append(evs, Event{
					Kind:     "ticket-expired",
					Resource: id,
					At:       now,
					Token:    tk.unitID,
				})
				continue
			}
			kept = append(kept, tk)
		}
		if len(kept) == 0 {
			delete(e.tickets, id)
		} else {
			e.tickets[id] = kept
		}
	}
	return evs
}

func (e *Engine) wakeWaitersLocked(now time.Time) []Event {
	var evs []Event
	for _, tok := range sortedMapKeys(e.waiters) {
		w := e.waiters[tok]
		if w == nil || w.wakeAt.After(now) {
			continue
		}
		evs = append(evs, Event{
			Kind:     "wake",
			Resource: w.resource,
			At:       now,
			Token:    w.token,
		})
		delete(e.waiters, tok)
	}
	return evs
}

// nextDue is the earliest resource timer, lease expiry, ticket expiry or
// waiter wake. The bool is false when nothing is armed.
func (e *Engine) nextDue() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var due time.Time
	ok := false
	note := func(t time.Time) {
		if !ok || t.Before(due) {
			due = t
			ok = true
		}
	}
	for _, st := range e.res {
		if st == nil {
			continue
		}
		if t, has := st.nextTimer(); has {
			note(t)
		}
	}
	for _, rec := range e.leases {
		if rec == nil {
			continue
		}
		note(rec.lease.ExpiresAt)
	}
	for _, ts := range e.tickets {
		for _, tk := range ts {
			if tk != nil {
				note(tk.expires)
			}
		}
	}
	for _, w := range e.waiters {
		if w != nil {
			note(w.wakeAt)
		}
	}
	return due, ok
}

// Run processes timers until ctx is cancelled. Waits use the real clock for
// due.Sub(e.now()) and never exceed 250ms, so a timer armed during a sleep
// is picked up on the next iteration.
func (e *Engine) Run(ctx context.Context) error {
	idle := time.NewTicker(runPoll)
	defer idle.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		due, ok := e.nextDue()
		if !ok {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-idle.C:
			}
			continue
		}
		wait := due.Sub(e.now())
		if wait > runPoll {
			wait = runPoll
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			e.Advance(e.now())
		}
	}
}

// kickTimers is the hook acquire and settle call after arming a sooner timer.
// There is no kick channel on Engine; Run's 250ms poll bound is the wake.
func (e *Engine) kickTimers() {}

func copyAmounts(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
