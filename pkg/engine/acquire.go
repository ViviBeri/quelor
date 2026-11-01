package engine

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/resource"
	"github.com/coherera/quelor/pkg/trace"
)

// acquireOut carries what a finished acquire needs to trace and return.
type acquireOut struct {
	verdict  quelor.Verdict
	lease    *quelor.Lease
	dref     string
	resp     decider.Response
	readings []quelor.Reading
	traceID  string
	lat      float64
}

// AcquireLease runs admission for one envelope: authenticate the holder, read
// the demanded resources, consult the decider (without e.mu held), then apply
// meta-admission (coalescing, quota, tickets) and reserve every demand
// atomically. Every outcome, grant or refusal, is traced with the envelope
// hash and the decider's ruleset hash. A refusal is a typed verdict, not an
// error; errors are reserved for engine faults.
func (e *Engine) AcquireLease(ctx context.Context, req quelor.AcquireRequest) (quelor.AcquireResult, error) {
	start := e.now()
	ctx, span := e.tracer.Start(ctx, "quelor.acquire")
	defer span.End()
	env := req.Envelope
	env.Demands = settleCloneI64(req.Envelope.Demands)
	span.SetAttributes(
		attribute.String("quelor.op", string(trace.OpAcquire)),
		attribute.String("quelor.unit_id", env.UnitID),
		attribute.String("quelor.holder_id", env.HolderID),
		attribute.String("quelor.tenant_id", env.TenantID),
	)
	traceID := ""
	if sc := span.SpanContext(); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}
	finish := func(o acquireOut) (quelor.AcquireResult, error) {
		if o.dref == "" {
			o.dref = randHex(16)
		}
		if o.traceID == "" {
			o.traceID = traceID
		}
		e.countVerdict(o.verdict.Kind)
		e.writeAcquireTrace(env, o)
		span.SetAttributes(attribute.String("quelor.verdict", string(o.verdict.Kind)))
		return quelor.AcquireResult{Verdict: o.verdict, Lease: o.lease, DecisionRef: o.dref}, nil
	}
	refuse := func(kind quelor.VerdictKind, reason string, stamps ...string) (quelor.AcquireResult, error) {
		return finish(acquireOut{
			verdict: quelor.Verdict{Kind: kind, Reason: reason, Stamps: settleNonEmpty(stamps)},
			lat:     float64(e.now().Sub(start)) / float64(time.Millisecond),
		})
	}

	if !e.authenticate(env.HolderID, req.Token) {
		return refuse(quelor.Deny, "unauthenticated-holder")
	}
	if env.UnitID == "" {
		return refuse(quelor.Deny, "invalid-envelope:unit_id")
	}
	for _, id := range settleSortedKeys(env.Demands) {
		if env.Demands[id] <= 0 {
			return refuse(quelor.Deny, "invalid-demand:"+id)
		}
		if _, ok := e.cfg.Registry.Manifest(id); !ok {
			return refuse(quelor.Deny, "unknown-resource:"+id)
		}
	}
	if env.Deadline != nil && !env.Deadline.After(start) {
		return refuse(quelor.Shed, "deadline-passed")
	}

	// A unit that already holds a lease is replayed, never double-reserved.
	e.mu.Lock()
	if out, ok := e.replayLocked(env); ok {
		e.mu.Unlock()
		return finish(out)
	}
	e.mu.Unlock()

	ids := settleSortedKeys(env.Demands)
	firstID, firstNeed := "", int64(0) // wake bookkeeping for decider-driven DEFER
	if len(ids) > 0 {
		firstID, firstNeed = ids[0], env.Demands[ids[0]]
	}
	readings, external := e.acquireReadings(ctx, ids, start)
	resp, derr := e.consult(ctx, decider.Request{
		Op:       decider.OpAcquire,
		Envelope: env,
		Readings: readings,
		Meta: decider.CallMeta{
			TraceID: traceID,
			Now:     start.UTC().Format(time.RFC3339Nano),
		},
	})

	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	lat := func() float64 { return float64(now.Sub(start)) / float64(time.Millisecond) }
	stamps := append([]string{}, resp.Verdict.Stamps...)
	out := acquireOut{resp: resp, readings: readings, traceID: traceID, dref: resp.DecisionRef}
	done := func(v quelor.Verdict, l *quelor.Lease) (quelor.AcquireResult, error) {
		v.Stamps = settleNonEmpty(append(stamps, v.Stamps...))
		out.verdict, out.lease, out.lat = v, l, lat()
		return finish(out)
	}

	if o, ok := e.replayLocked(env); ok {
		return finish(o)
	}

	if unreachable(resp, derr) {
		// Fail static: a spend demand is never admitted on a guess, and an
		// unknown-policy demand yields only to an operator directive.
		e.st.boundUnreachable.Add(1)
		if !settleHasStamp(stamps, "bound-unreachable") {
			stamps = append(stamps, "bound-unreachable")
		}
		if kind, refused := e.acquireUnreachableRefusal(env, ids); refused {
			return done(e.deferOrDeny(kind, "bound-unreachable", firstID, firstNeed, nil, now), nil)
		}
	} else if resp.Enforced() && resp.Verdict.Kind != quelor.Admit {
		v := resp.Verdict
		if v.Reason == "" {
			v.Reason = resp.DecisionRef
		}
		if v.Reason == "" {
			v.Reason = string(v.Kind)
		}
		if v.Kind == quelor.Defer {
			v = e.deferOrDeny(quelor.Defer, v.Reason, firstID, firstNeed, v.WakeAt, now)
		}
		v.Stamps = nil // decider stamps are added once, in done
		return done(v, nil)
	}

	// Unknown and stale readings are never capacity. hold_cursor lets a
	// local-only resource continue; every other policy refuses.
	for _, id := range ids {
		rd := readingFor(readings, id)
		if rd.Usable() {
			if external[id] && rd.Available < env.Demands[id] {
				return done(e.contend(env, id, now, "insufficient:"+id), nil)
			}
			continue
		}
		m, _ := e.cfg.Registry.Manifest(id)
		switch kind := m.UnknownPolicy.Verdict(); kind {
		case quelor.Admit:
		case quelor.Deny:
			return done(quelor.Verdict{Kind: kind, Reason: "unknown-reading:" + id}, nil)
		default:
			return done(e.deferOrDeny(kind, "unknown-reading:"+id, id, env.Demands[id], nil, now), nil)
		}
	}

	// Coalescing.
	var superseded *leaseRec
	if key := env.CoalesceKey; key != "" {
		if oldID, ok := e.byKey[key]; ok {
			if old := e.leases[oldID]; old != nil && old.env.UnitID != env.UnitID {
				if env.CoalescePolicy == quelor.CoalesceDropNew {
					return done(quelor.Verdict{Kind: quelor.Shed, Reason: "coalesced:drop-new"}, nil)
				}
				if env.Generation < old.env.Generation {
					return done(quelor.Verdict{Kind: quelor.Shed, Reason: "coalesced:stale-generation"}, nil)
				}
				superseded = old
			}
		}
	}

	if reason := e.acquireQuotaReason(env); reason != "" {
		return done(quelor.Verdict{Kind: quelor.Deny, Reason: reason}, nil)
	}

	// Capacity: queue behind better-ranked tickets, then check, then take.
	if id, other := e.ticketBlockLocked(env, ids, now); other != "" {
		return done(e.contend(env, id, now, "queued-behind:"+other), nil)
	}
	for _, id := range ids {
		if s := e.res[id]; s == nil || s.avail(now) < env.Demands[id] {
			return done(e.contend(env, id, now, "insufficient:"+id), nil)
		}
	}

	rec := &leaseRec{
		env:     env,
		windows: map[string]uint64{},
		probe:   map[string]bool{},
	}
	for _, id := range ids {
		s := e.res[id]
		if s.model == resource.ModelCircuit && s.circ == modeHalfOpen {
			rec.probe[id] = true
		}
		s.take(env.Demands[id], now)
		rec.windows[id] = s.windowID()
	}
	dref := resp.DecisionRef
	if dref == "" {
		dref = randHex(16)
	}
	out.dref = dref
	rec.lease = quelor.Lease{
		ID:          randHex(16),
		UnitID:      env.UnitID,
		TenantID:    env.TenantID,
		HolderID:    env.HolderID,
		Declared:    settleCloneI64(env.Demands),
		GrantedAt:   now,
		ExpiresAt:   now.Add(e.cfg.LeaseTTL),
		DecisionRef: dref,
	}
	e.leases[rec.lease.ID] = rec
	e.byUnit[env.UnitID] = rec.lease.ID
	if env.CoalesceKey != "" {
		e.byKey[env.CoalesceKey] = rec.lease.ID
	}
	if superseded != nil {
		superseded.superseded = true
		stamps = append(stamps, "superseded:"+superseded.lease.ID)
	}
	h := e.holders[env.HolderID]
	if h == nil {
		h = &holderUse{units: map[string]int64{}}
		e.holders[env.HolderID] = h
	}
	if h.units == nil {
		h.units = map[string]int64{}
	}
	h.leases++
	for id, n := range env.Demands {
		h.units[id] += n
	}
	e.clearTicketsLocked(env.UnitID)
	e.kickTimers()

	leaseOut := settleCloneLease(rec.lease)
	return done(quelor.Verdict{Kind: quelor.Admit}, &leaseOut)
}

// replayLocked returns the existing lease when env.UnitID already holds one.
func (e *Engine) replayLocked(env quelor.Envelope) (acquireOut, bool) {
	id, ok := e.byUnit[env.UnitID]
	if !ok {
		return acquireOut{}, false
	}
	rec := e.leases[id]
	if rec == nil {
		return acquireOut{}, false
	}
	if rec.lease.HolderID != env.HolderID {
		return acquireOut{
			verdict: quelor.Verdict{Kind: quelor.Deny, Reason: "unit-held-by-other-holder"},
		}, true
	}
	l := settleCloneLease(rec.lease)
	return acquireOut{
		verdict: quelor.Verdict{Kind: quelor.Admit, Stamps: []string{"replay"}},
		lease:   &l,
		dref:    rec.lease.DecisionRef,
	}, true
}

// acquireReadings reads every demanded resource. external marks resources
// whose reading came from a registered reader rather than engine state.
func (e *Engine) acquireReadings(ctx context.Context, ids []string, now time.Time) ([]quelor.Reading, map[string]bool) {
	external := map[string]bool{}
	out := make([]quelor.Reading, 0, len(ids))
	for _, id := range ids {
		rd, err := e.cfg.Registry.Read(ctx, id)
		switch {
		case err == nil:
			external[id] = true
			out = append(out, rd)
		case errors.Is(err, resource.ErrNoReader):
			e.mu.Lock()
			s := e.res[id]
			local := quelor.Reading{Resource: id, ObservedAt: now}
			if s != nil {
				local.Available, local.Capacity, local.Known = s.avail(now), s.effective(), true
			}
			e.mu.Unlock()
			out = append(out, local)
		default:
			rd.Resource = id
			rd.Known = false
			external[id] = true
			out = append(out, rd)
		}
	}
	return out, external
}

func readingFor(rs []quelor.Reading, id string) quelor.Reading {
	for _, r := range rs {
		if r.Resource == id {
			return r
		}
	}
	return quelor.Reading{Resource: id}
}

// acquireUnreachableRefusal mirrors renewUnreachableMustRefuse for a whole
// envelope. A hard budget always refuses; an unknown-policy demand refuses
// unless the unit is an operator directive.
func (e *Engine) acquireUnreachableRefusal(env quelor.Envelope, ids []string) (quelor.VerdictKind, bool) {
	kind := quelor.Defer
	policyRefuse := false
	for _, id := range ids {
		m, ok := e.cfg.Registry.Manifest(id)
		if !ok {
			continue
		}
		if m.CapacityModel == resource.ModelHardBudget {
			return quelor.Defer, true
		}
		switch m.UnknownPolicy {
		case resource.UnknownDeferShort, resource.UnknownAssumeBusy:
			policyRefuse = true
		case resource.UnknownDeny:
			policyRefuse = true
			kind = quelor.Deny
		}
	}
	if env.Priority == quelor.PriorityOperatorDirective {
		return kind, false
	}
	return kind, policyRefuse
}

func (e *Engine) acquireQuotaReason(env quelor.Envelope) string {
	q := e.quotaFor(env.HolderID)
	h := e.holders[env.HolderID]
	if q.MaxLeases > 0 && h != nil && h.leases >= q.MaxLeases {
		return "holder-quota-exceeded"
	}
	for _, id := range settlePositiveKeys(env.Demands) {
		limit, ok := q.MaxUnits[id]
		if !ok || limit <= 0 {
			continue
		}
		var used int64
		if h != nil && h.units != nil {
			used = h.units[id]
		}
		if used+env.Demands[id] > limit {
			return "holder-quota-exceeded"
		}
	}
	return ""
}

// deferOrDeny builds a DEFER (with a wake token waiter) or any other kind.
func (e *Engine) deferOrDeny(kind quelor.VerdictKind, reason, res string, need int64, wake *time.Time, now time.Time) quelor.Verdict {
	v := quelor.Verdict{Kind: kind, Reason: reason}
	if kind != quelor.Defer {
		return v
	}
	var at time.Time
	switch {
	case wake != nil:
		at = *wake
	default:
		at = now.Add(e.cfg.AgingStep)
		if s := e.res[res]; s != nil {
			if h := s.wakeHint(need, now); h.After(now) {
				at = h
			}
		}
	}
	token := randHex(8)
	e.waiters[token] = &waiter{token: token, resource: res, need: need, wakeAt: at}
	v.WakeAt = &at
	v.WakeToken = token
	return v
}

// contend defers a unit that lost on resource id, and holds its ticket there so
// better-ranked work is not barged by later arrivals.
func (e *Engine) contend(env quelor.Envelope, id string, now time.Time, reason string) quelor.Verdict {
	need := env.Demands[id]
	if s := e.res[id]; s != nil && s.model == resource.ModelCircuit {
		return e.deferOrDeny(quelor.Defer, reason, id, need, nil, now)
	}
	rank := env.Priority.Rank()
	since := now
	for _, tk := range e.tickets[id] {
		if tk.unitID == env.UnitID {
			since = tk.since
		}
	}
	kept := e.tickets[id][:0:0]
	for _, tk := range e.tickets[id] {
		if tk.unitID != env.UnitID {
			kept = append(kept, tk)
		}
	}
	e.tickets[id] = append(kept, &ticket{
		unitID: env.UnitID, need: need, rank: rank, since: since, expires: now.Add(e.cfg.TicketTTL),
	})
	var wake *time.Time
	return e.deferOrDeny(quelor.Defer, reason, id, need, wake, now)
}

// effRank is the ticket rank after aging: one step better per AgingStep waited.
func (e *Engine) effRank(rank int, since, now time.Time) int {
	r := rank - int(now.Sub(since)/e.cfg.AgingStep)
	if r < 0 {
		r = 0
	}
	return r
}

// ticketBlockLocked reports the resource and unit of a live ticket that outranks
// env and still needs room that this admission would take.
func (e *Engine) ticketBlockLocked(env quelor.Envelope, ids []string, now time.Time) (string, string) {
	mine := env.Priority.Rank()
	for _, id := range ids {
		s := e.res[id]
		// A breaker has one probe slot and no queue to order.
		if s == nil || s.model == resource.ModelCircuit {
			continue
		}
		for _, tk := range e.tickets[id] {
			if tk.unitID == env.UnitID || !tk.expires.After(now) {
				continue
			}
			if e.effRank(tk.rank, tk.since, now) >= e.effRank(mine, now, now) {
				continue
			}
			if s.avail(now)-env.Demands[id] < tk.need {
				return id, tk.unitID
			}
		}
	}
	return "", ""
}

func (e *Engine) clearTicketsLocked(unit string) {
	for id, ts := range e.tickets {
		kept := ts[:0:0]
		for _, tk := range ts {
			if tk.unitID != unit {
				kept = append(kept, tk)
			}
		}
		if len(kept) == 0 {
			delete(e.tickets, id)
		} else {
			e.tickets[id] = kept
		}
	}
}

func (e *Engine) writeAcquireTrace(env quelor.Envelope, o acquireOut) {
	v := o.verdict
	var leaseID, holder, tenant string
	var declared map[string]int64
	holder, tenant = env.HolderID, env.TenantID
	if o.lease != nil {
		leaseID = o.lease.ID
		declared = settleCloneI64(o.lease.Declared)
	}
	status := settleTraceStatus(o.resp.Status)
	var mode trace.Mode
	if o.resp.Status != "" {
		mode = settleTraceMode(o.resp.Mode)
	}
	e.writeTrace(trace.Record{
		Op:            trace.OpAcquire,
		DecisionRef:   o.dref,
		TraceID:       o.traceID,
		EnvelopeHash:  trace.HashEnvelope(env),
		Envelope:      &env,
		HolderID:      holder,
		TenantID:      tenant,
		LeaseID:       leaseID,
		Verdict:       &v,
		Readings:      o.readings,
		RulesetID:     o.resp.RulesetID,
		RulesetHash:   o.resp.RulesetHash,
		DeciderID:     o.resp.DeciderID,
		RuleIDs:       append([]string{}, o.resp.RuleIDs...),
		Provenance:    trace.Provenance{Step: o.resp.Provenance.Step, Tier: o.resp.Provenance.Tier, EvidenceClass: o.resp.Provenance.EvidenceClass},
		DeciderStatus: status,
		Mode:          mode,
		Stamps:        append([]string{}, v.Stamps...),
		Declared:      declared,
		LatencyMS:     o.lat,
	})
}
