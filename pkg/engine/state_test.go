package engine

import (
	"math"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/resource"
)

func epoch() time.Time {
	return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
}

func mk(model resource.CapacityModel, capacity, min int64, window, openFor time.Duration, threshold int) resource.Manifest {
	var m resource.Manifest
	m.ID = "cpu"
	m.CapacityModel = model
	m.Capacity = capacity
	m.Window = resource.Duration(window)
	m.MinCapacity = min
	m.FailureThreshold = threshold
	m.OpenFor = resource.Duration(openFor)
	return m
}

func assertKinds(t *testing.T, ev []Event, id string, want []string) {
	t.Helper()
	if len(ev) != len(want) {
		got := make([]string, len(ev))
		for i, e := range ev {
			got[i] = e.Kind
		}
		t.Fatalf("kinds=%v want %v", got, want)
	}
	for i := range want {
		if ev[i].Kind != want[i] {
			t.Fatalf("kind[%d]=%s want %s", i, ev[i].Kind, want[i])
		}
		if ev[i].Resource != id {
			t.Fatalf("resource[%d]=%s want %s", i, ev[i].Resource, id)
		}
		if ev[i].At.IsZero() {
			t.Fatalf("event %d has zero time", i)
		}
	}
}

func TestTakeAvailGiveBack(t *testing.T) {
	origin := epoch()
	type row struct {
		name   string
		model  resource.CapacityModel
		take1  int64
		ok1    bool
		avail1 int64
		take2  int64
		ok2    bool
		avail2 int64
		decl   int64
		cons   int64
		win    uint64
		avail3 int64
	}
	rows := []row{
		{name: "lease-ignores-consumed", model: resource.ModelLease, take1: 4, ok1: true, avail1: 6, take2: 7, ok2: false, avail2: 6, decl: 4, cons: 100, win: 7, avail3: 10},
		{name: "lease-exact", model: resource.ModelLease, take1: 10, ok1: true, avail1: 0, take2: 1, ok2: false, avail2: 0, decl: 10, cons: 10, win: 0, avail3: 10},
		{name: "hard-partial-refund", model: resource.ModelHardBudget, take1: 4, ok1: true, avail1: 6, take2: 6, ok2: true, avail2: 0, decl: 6, cons: 1, win: 99, avail3: 5},
		{name: "hard-no-refund-over-consume", model: resource.ModelHardBudget, take1: 4, ok1: true, avail1: 6, take2: 1, ok2: true, avail2: 5, decl: 1, cons: 8, win: 0, avail3: 5},
		{name: "hard-refund-clamps", model: resource.ModelHardBudget, take1: 2, ok1: true, avail1: 8, take2: 0, ok2: false, avail2: 8, decl: 9, cons: 0, win: 0, avail3: 10},
		{name: "window-same-generation", model: resource.ModelWindowBucket, take1: 4, ok1: true, avail1: 6, take2: 1, ok2: true, avail2: 5, decl: 4, cons: 1, win: 0, avail3: 8},
		{name: "window-wrong-generation", model: resource.ModelWindowBucket, take1: 4, ok1: true, avail1: 6, take2: 1, ok2: true, avail2: 5, decl: 5, cons: 0, win: 1, avail3: 5},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newResState(mk(row.model, 10, 1, time.Minute, 0, 1), origin)
			if s.effective() != 10 || s.windowID() != 0 {
				t.Fatalf("init eff=%d win=%d", s.effective(), s.windowID())
			}
			if s.avail(origin) != 10 {
				t.Fatalf("fresh avail=%d", s.avail(origin))
			}
			if got := s.take(-3, origin); got {
				t.Fatal("negative take succeeded")
			}
			if got := s.take(row.take1, origin); got != row.ok1 {
				t.Fatalf("take1=%v want %v", got, row.ok1)
			}
			if s.avail(origin) != row.avail1 {
				t.Fatalf("avail1=%d want %d", s.avail(origin), row.avail1)
			}
			if row.take2 > 0 {
				if got := s.take(row.take2, origin); got != row.ok2 {
					t.Fatalf("take2=%v want %v", got, row.ok2)
				}
			}
			if s.avail(origin) != row.avail2 {
				t.Fatalf("avail2=%d want %d", s.avail(origin), row.avail2)
			}
			// A rejected take must not move the counters.
			before := s.avail(origin)
			if s.take(1<<62, origin) {
				t.Fatal("oversize take succeeded")
			}
			if s.avail(origin) != before {
				t.Fatalf("failed take changed avail to %d", s.avail(origin))
			}
			s.giveBack(row.decl, row.cons, row.win, origin)
			if s.avail(origin) != row.avail3 {
				t.Fatalf("avail3=%d want %d", s.avail(origin), row.avail3)
			}
			if s.avail(origin) < 0 {
				t.Fatal("negative avail")
			}
		})
	}
}

func TestAddReservation(t *testing.T) {
	origin := epoch()
	s := newResState(mk(resource.ModelLease, 10, 1, 0, 0, 1), origin)
	if !s.take(3, origin) || s.avail(origin) != 7 {
		t.Fatalf("avail=%d", s.avail(origin))
	}
	if !s.addReservation(4, origin) || s.avail(origin) != 3 {
		t.Fatalf("after add avail=%d", s.avail(origin))
	}
	if s.addReservation(4, origin) {
		t.Fatal("oversize add succeeded")
	}
	if s.avail(origin) != 3 {
		t.Fatalf("rejected add changed avail to %d", s.avail(origin))
	}
	if !s.addReservation(3, origin) || s.avail(origin) != 0 {
		t.Fatalf("final avail=%d", s.avail(origin))
	}
	if s.probeSettled(false, origin) != nil {
		t.Fatal("probeSettled on lease")
	}
}

func TestHardBudgetNeverResets(t *testing.T) {
	origin := epoch()
	s := newResState(mk(resource.ModelHardBudget, 10, 1, time.Hour, 0, 1), origin)
	if !s.take(4, origin) {
		t.Fatal("take")
	}
	ev := s.advance(origin.Add(48 * time.Hour))
	if len(ev) != 0 {
		t.Fatalf("events=%v", ev)
	}
	if _, ok := s.nextTimer(); ok {
		t.Fatal("hard budget has no timer")
	}
	if s.avail(origin.Add(48*time.Hour)) != 6 {
		t.Fatalf("avail=%d", s.avail(origin))
	}
	s.giveBack(4, 1, 123, origin.Add(48*time.Hour))
	if s.avail(origin) != 9 {
		t.Fatalf("avail after refund=%d", s.avail(origin))
	}
}

func TestWindowResetAndRefund(t *testing.T) {
	origin := epoch()
	window := time.Minute

	t.Run("just-before-boundary", func(t *testing.T) {
		s := newResState(mk(resource.ModelWindowBucket, 10, 1, window, 0, 1), origin)
		if !s.take(6, origin) {
			t.Fatal("take")
		}
		now := origin.Add(window - time.Nanosecond)
		ev := s.advance(now)
		assertKinds(t, ev, "cpu", nil)
		if s.windowID() != 0 || s.avail(now) != 4 {
			t.Fatalf("id=%d avail=%d", s.windowID(), s.avail(now))
		}
		s.giveBack(6, 2, 0, now)
		if s.avail(now) != 8 || s.windowID() != 0 {
			t.Fatalf("refund avail=%d id=%d", s.avail(now), s.windowID())
		}
		got, ok := s.nextTimer()
		if !ok || !got.Equal(origin.Add(window)) {
			t.Fatalf("timer=%s ok=%v", got, ok)
		}
	})

	t.Run("exact-and-ignore-old", func(t *testing.T) {
		s := newResState(mk(resource.ModelWindowBucket, 10, 1, window, 0, 1), origin)
		if !s.take(6, origin) {
			t.Fatal("take")
		}
		now := origin.Add(window)
		ev := s.advance(now)
		assertKinds(t, ev, "cpu", []string{"window-reset"})
		if !ev[0].At.Equal(now) {
			t.Fatalf("reset at %s", ev[0].At)
		}
		if s.windowID() != 1 || s.avail(now) != 10 {
			t.Fatalf("id=%d avail=%d", s.windowID(), s.avail(now))
		}
		again := s.advance(now)
		assertKinds(t, again, "cpu", nil)
		if s.windowID() != 1 {
			t.Fatalf("id changed to %d", s.windowID())
		}
		s.giveBack(6, 0, 0, now)
		if s.avail(now) != 10 {
			t.Fatal("old generation refunded")
		}
		if !s.take(3, now) || s.avail(now) != 7 {
			t.Fatalf("avail=%d", s.avail(now))
		}
		s.giveBack(6, 0, 0, now)
		if s.avail(now) != 7 {
			t.Fatal("stale refund applied")
		}
		s.giveBack(3, 3, 1, now)
		if s.avail(now) != 7 {
			t.Fatalf("zero refund avail=%d", s.avail(now))
		}
		s.giveBack(3, 1, 1, now)
		if s.avail(now) != 9 {
			t.Fatalf("avail=%d want 9", s.avail(now))
		}
		got, ok := s.nextTimer()
		if !ok || !got.Equal(origin.Add(2*window)) {
			t.Fatalf("timer=%s", got)
		}
	})

	t.Run("catch-up-multiple", func(t *testing.T) {
		s := newResState(mk(resource.ModelWindowBucket, 10, 1, window, 0, 1), origin)
		if !s.take(6, origin) {
			t.Fatal("take")
		}
		now := origin.Add(3*window + time.Nanosecond)
		ev := s.advance(now)
		if len(ev) != 3 {
			t.Fatalf("events=%d", len(ev))
		}
		for i := range ev {
			if ev[i].Kind != "window-reset" || ev[i].Resource != "cpu" {
				t.Fatalf("event %+v", ev[i])
			}
			wantAt := origin.Add(time.Duration(i+1) * window)
			if !ev[i].At.Equal(wantAt) {
				t.Fatalf("event %d at %s want %s", i, ev[i].At, wantAt)
			}
		}
		if s.windowID() != 3 || s.avail(now) != 10 {
			t.Fatalf("id=%d avail=%d", s.windowID(), s.avail(now))
		}
		if s.advance(now) != nil || s.windowID() != 3 {
			t.Fatal("advance not idempotent")
		}
		s.giveBack(6, 0, 0, now)
		if !s.take(4, now) || s.avail(now) != 6 {
			t.Fatalf("avail=%d", s.avail(now))
		}
		s.giveBack(6, 0, 0, now)
		if s.avail(now) != 6 {
			t.Fatal("pre-reset refund applied after catch-up")
		}
		s.giveBack(4, 0, s.windowID(), now)
		if s.avail(now) != 10 {
			t.Fatalf("current refund avail=%d", s.avail(now))
		}
		got, ok := s.nextTimer()
		if !ok || !got.Equal(origin.Add(4*window)) {
			t.Fatalf("timer=%s", got)
		}
	})
}

func TestGCRA(t *testing.T) {
	origin := epoch()
	s := newResState(mk(resource.ModelGCRA, 10, 1, 10*time.Second, 0, 1), origin)
	if s.avail(origin) != 10 {
		t.Fatalf("fresh=%d", s.avail(origin))
	}
	if !s.take(10, origin) || s.avail(origin) != 0 {
		t.Fatal("drain")
	}
	if s.take(1, origin) {
		t.Fatal("empty take")
	}
	t1 := origin.Add(1500 * time.Millisecond)
	if s.avail(t1) != 1 {
		t.Fatalf("floor avail=%d want 1", s.avail(t1))
	}
	if !s.take(1, t1) || s.avail(t1) != 0 {
		t.Fatalf("after 1 avail=%d", s.avail(t1))
	}
	if s.take(1, t1) {
		t.Fatal("fractional token was treated as a whole token")
	}
	t2 := origin.Add(2 * time.Second)
	if s.avail(t2) != 1 {
		t.Fatalf("at 2s avail=%d", s.avail(t2))
	}
	if !s.addReservation(1, t2) || s.avail(t2) != 0 {
		t.Fatal("addReservation")
	}
	s.giveBack(1, 0, 99, t2)
	if s.avail(t2) != 1 {
		t.Fatalf("refund avail=%d", s.avail(t2))
	}
	s.giveBack(1, 5, 0, t2)
	if s.avail(t2) != 1 {
		t.Fatalf("over-consume changed avail to %d", s.avail(t2))
	}
	s.giveBack(100, 0, 0, t2)
	if s.avail(t2) != 10 {
		t.Fatalf("cap avail=%d", s.avail(t2))
	}
	far := t2.Add(1000 * time.Second)
	if s.avail(far) != 10 {
		t.Fatalf("long refill avail=%d", s.avail(far))
	}
	if _, ok := s.nextTimer(); ok {
		t.Fatal("gcra has no timer without a block")
	}

	t.Run("rate-uses-current-eff", func(t *testing.T) {
		g := newResState(mk(resource.ModelGCRA, 100, 1, 10*time.Second, 0, 1), origin)
		if !g.take(100, origin) {
			t.Fatal("drain")
		}
		ev := g.observe(503, 0, origin)
		assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
		if !ev[0].At.Equal(origin) || g.effective() != 50 || g.avail(origin) != 0 {
			t.Fatalf("eff=%d avail=%d", g.effective(), g.avail(origin))
		}
		if g.avail(origin.Add(10*time.Second)) != 50 {
			t.Fatalf("refilled avail=%d want 50", g.avail(origin.Add(10*time.Second)))
		}
		if !g.take(50, origin.Add(10*time.Second)) {
			t.Fatal("take refilled")
		}
		if g.avail(origin.Add(12*time.Second)) != 10 {
			t.Fatalf("avail=%d want 10 at new rate", g.avail(origin.Add(12*time.Second)))
		}
	})

	t.Run("decrease-clamps-tokens", func(t *testing.T) {
		g := newResState(mk(resource.ModelGCRA, 100, 1, 10*time.Second, 0, 1), origin)
		ev := g.observe(503, 0, origin)
		assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
		if g.effective() != 50 || g.avail(origin) != 50 {
			t.Fatalf("eff=%d avail=%d", g.effective(), g.avail(origin))
		}
		ev = g.observe(200, 0, origin)
		assertKinds(t, ev, "cpu", []string{"aimd-increase"})
		if g.effective() != 55 || g.avail(origin) != 50 {
			t.Fatalf("increase granted tokens eff=%d avail=%d", g.effective(), g.avail(origin))
		}
	})
}

func TestCircuitCycle(t *testing.T) {
	origin := epoch()
	huge := int64(math.MaxInt64 / 2)
	s := newResState(mk(resource.ModelCircuit, 80, 1, 0, 10*time.Second, 2), origin)
	type circStep struct {
		name   string
		at     time.Duration
		op     string
		status int
		n      int64
		pok    bool
		tok    bool
		avail  int64
		kinds  []string
		hasT   bool
		tend   time.Duration
	}
	steps := []circStep{
		{name: "closed", op: "avail", avail: huge},
		{name: "probe-idle", op: "probe", pok: false, avail: huge},
		{name: "fail-1", op: "obs", status: 500, avail: huge},
		{name: "success-resets", at: time.Second, op: "obs", status: 200, avail: huge},
		{name: "fail-again", at: 2 * time.Second, op: "obs", status: 500, avail: huge},
		{name: "trip", at: 3 * time.Second, op: "obs", status: 500, avail: 0, kinds: []string{"circuit-open"}, hasT: true, tend: 13 * time.Second},
		{name: "take-open", at: 4 * time.Second, op: "take", n: 1, tok: false, avail: 0, hasT: true, tend: 13 * time.Second},
		{name: "early", at: 12 * time.Second, op: "adv", avail: 0, hasT: true, tend: 13 * time.Second},
		{name: "half", at: 13 * time.Second, op: "adv", avail: 1, kinds: []string{"half-open"}},
		{name: "half-idempotent", at: 13 * time.Second, op: "adv", avail: 1},
		{name: "take-too-big", at: 13 * time.Second, op: "take", n: 2, tok: false, avail: 1},
		{name: "probe-take", at: 13 * time.Second, op: "add", n: 1, tok: true, avail: 0},
		{name: "second-probe", at: 13 * time.Second, op: "take", n: 1, tok: false, avail: 0},
		{name: "probe-fail", at: 14 * time.Second, op: "probe", pok: false, avail: 0, kinds: []string{"circuit-open"}, hasT: true, tend: 24 * time.Second},
		{name: "half-2", at: 24 * time.Second, op: "adv", avail: 1, kinds: []string{"half-open"}},
		{name: "probe-take-2", at: 24 * time.Second, op: "take", n: 1, tok: true, avail: 0},
		{name: "probe-ok", at: 25 * time.Second, op: "probe", pok: true, avail: huge, kinds: []string{"circuit-closed"}},
		{name: "429-under", at: 26 * time.Second, op: "obs", status: 429, avail: huge},
		{name: "429-trip", at: 27 * time.Second, op: "obs", status: 429, avail: 0, kinds: []string{"circuit-open"}, hasT: true, tend: 37 * time.Second},
		{name: "half-3", at: 37 * time.Second, op: "adv", avail: 1, kinds: []string{"half-open"}},
		{name: "probe-take-3", at: 37 * time.Second, op: "take", n: 1, tok: true, avail: 0},
		{name: "obs-fail", at: 37 * time.Second, op: "obs", status: 500, avail: 0, kinds: []string{"circuit-open"}, hasT: true, tend: 47 * time.Second},
		{name: "settled-ignored", at: 38 * time.Second, op: "probe", pok: true, avail: 0, hasT: true, tend: 47 * time.Second},
		{name: "half-4", at: 47 * time.Second, op: "adv", avail: 1, kinds: []string{"half-open"}},
		{name: "probe-take-4", at: 47 * time.Second, op: "take", n: 1, tok: true, avail: 0},
		{name: "obs-ok", at: 47 * time.Second, op: "obs", status: 204, avail: huge, kinds: []string{"circuit-closed"}},
		{name: "settled-ignored-2", at: 48 * time.Second, op: "probe", pok: false, avail: huge},
		{name: "closed-take", at: 48 * time.Second, op: "take", n: 7, tok: true, avail: huge},
		{name: "fail-after-close", at: 49 * time.Second, op: "obs", status: 500, avail: huge},
		{name: "400-resets", at: 50 * time.Second, op: "obs", status: 400, avail: huge},
		{name: "fail-after-400", at: 51 * time.Second, op: "obs", status: 500, avail: huge},
		{name: "502-trips", at: 52 * time.Second, op: "obs", status: 502, avail: 0, kinds: []string{"circuit-open"}, hasT: true, tend: 62 * time.Second},
		{name: "open-ignores-200", at: 53 * time.Second, op: "obs", status: 200, avail: 0, hasT: true, tend: 62 * time.Second},
	}
	for _, st := range steps {
		now := origin.Add(st.at)
		var ev []Event
		switch st.op {
		case "avail":
		case "obs":
			ev = s.observe(st.status, time.Second, now)
		case "take":
			if got := s.take(st.n, now); got != st.tok {
				t.Fatalf("%s: take=%v want %v", st.name, got, st.tok)
			}
		case "add":
			if got := s.addReservation(st.n, now); got != st.tok {
				t.Fatalf("%s: add=%v want %v", st.name, got, st.tok)
			}
		case "adv":
			ev = s.advance(now)
		case "probe":
			ev = s.probeSettled(st.pok, now)
		default:
			t.Fatalf("bad op %s", st.op)
		}
		assertKinds(t, ev, "cpu", st.kinds)
		for _, e := range ev {
			if !e.At.Equal(now) {
				t.Fatalf("%s: event at %s want %s", st.name, e.At, now)
			}
		}
		if got := s.avail(now); got != st.avail {
			t.Fatalf("%s: avail=%d want %d", st.name, got, st.avail)
		}
		if s.effective() != 80 {
			t.Fatalf("%s: eff=%d", st.name, s.effective())
		}
		tm, ok := s.nextTimer()
		if ok != st.hasT {
			t.Fatalf("%s: timer ok=%v want %v (%s)", st.name, ok, st.hasT, tm)
		}
		if ok && !tm.Equal(origin.Add(st.tend)) {
			t.Fatalf("%s: timer=%s want %s", st.name, tm, origin.Add(st.tend))
		}
	}
}

func TestCircuitThresholdOne(t *testing.T) {
	origin := epoch()
	s := newResState(mk(resource.ModelCircuit, 4, 1, 0, 5*time.Second, 1), origin)
	ev := s.observe(500, 0, origin)
	assertKinds(t, ev, "cpu", []string{"circuit-open"})
	if s.avail(origin) != 0 {
		t.Fatal("open avail")
	}
	ev = s.advance(origin.Add(5 * time.Second))
	assertKinds(t, ev, "cpu", []string{"half-open"})
	if s.avail(origin.Add(5*time.Second)) != 1 {
		t.Fatal("half-open avail")
	}
}

func TestAIMD(t *testing.T) {
	origin := epoch()
	type obsCase struct {
		name   string
		pre    bool
		status int
		want   int64
		kind   string
	}
	cases := []obsCase{
		{name: "429", status: 429, want: 50, kind: "aimd-decrease"},
		{name: "503", status: 503, want: 50, kind: "aimd-decrease"},
		{name: "500-ignored", status: 500, want: 100},
		{name: "at-cap-200", status: 200, want: 100},
		{name: "at-cap-302", status: 302, want: 100},
		{name: "increase-200", pre: true, status: 200, want: 55, kind: "aimd-increase"},
		{name: "increase-204", pre: true, status: 204, want: 55, kind: "aimd-increase"},
		{name: "increase-302", pre: true, status: 302, want: 55, kind: "aimd-increase"},
		{name: "increase-399", pre: true, status: 399, want: 55, kind: "aimd-increase"},
		{name: "no-400", pre: true, status: 400, want: 50},
		{name: "no-404", pre: true, status: 404, want: 50},
		{name: "no-428", pre: true, status: 428, want: 50},
		{name: "no-500", pre: true, status: 500, want: 50},
		{name: "zero", status: 0, want: 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newResState(mk(resource.ModelLease, 100, 10, 0, 0, 1), origin)
			if c.pre {
				ev := s.observe(429, 0, origin)
				assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
			}
			ev := s.observe(c.status, 0, origin)
			var want []string
			if c.kind != "" {
				want = []string{c.kind}
			}
			assertKinds(t, ev, "cpu", want)
			for _, e := range ev {
				if !e.At.Equal(origin) {
					t.Fatalf("at %s", e.At)
				}
			}
			if s.effective() != c.want {
				t.Fatalf("eff=%d want %d", s.effective(), c.want)
			}
		})
	}

	t.Run("floor-and-recovery", func(t *testing.T) {
		s := newResState(mk(resource.ModelLease, 100, 10, 0, 0, 1), origin)
		halves := []int64{50, 25, 12, 10, 10}
		for i, want := range halves {
			ev := s.observe(429, 0, origin)
			assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
			if s.effective() != want {
				t.Fatalf("half %d eff=%d want %d", i, s.effective(), want)
			}
		}
		ups := []int64{15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85, 90, 95, 100}
		for i, want := range ups {
			ev := s.observe(200, 0, origin)
			assertKinds(t, ev, "cpu", []string{"aimd-increase"})
			if s.effective() != want {
				t.Fatalf("up %d eff=%d want %d", i, s.effective(), want)
			}
		}
		if s.observe(204, 0, origin) != nil || s.effective() != 100 {
			t.Fatalf("eff=%d", s.effective())
		}
		if s.avail(origin) != 100 {
			t.Fatalf("avail=%d", s.avail(origin))
		}
	})

	t.Run("each-model", func(t *testing.T) {
		models := []resource.CapacityModel{
			resource.ModelLease,
			resource.ModelHardBudget,
			resource.ModelWindowBucket,
			resource.ModelGCRA,
		}
		for _, model := range models {
			s := newResState(mk(model, 100, 10, time.Minute, 0, 1), origin)
			ev := s.observe(429, 0, origin)
			assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
			if s.effective() != 50 {
				t.Fatalf("%s eff=%d", model, s.effective())
			}
		}
		s := newResState(mk(resource.ModelCircuit, 100, 10, 0, time.Minute, 5), origin)
		ev := s.observe(503, time.Second, origin)
		assertKinds(t, ev, "cpu", nil)
		if s.effective() != 100 || s.avail(origin) != math.MaxInt64/2 {
			t.Fatalf("circuit eff=%d avail=%d", s.effective(), s.avail(origin))
		}
		if _, ok := s.nextTimer(); ok {
			t.Fatal("circuit retry-after armed a timer")
		}
	})

	t.Run("spent-above-eff", func(t *testing.T) {
		for _, model := range []resource.CapacityModel{resource.ModelHardBudget, resource.ModelWindowBucket, resource.ModelLease} {
			s := newResState(mk(model, 100, 1, time.Minute, 0, 1), origin)
			if !s.take(80, origin) {
				t.Fatal(model)
			}
			s.observe(503, 0, origin)
			if s.effective() != 50 || s.avail(origin) != 0 {
				t.Fatalf("%s eff=%d avail=%d", model, s.effective(), s.avail(origin))
			}
			if s.take(1, origin) {
				t.Fatal("take under shrunken eff")
			}
		}
	})

	t.Run("small-steps", func(t *testing.T) {
		s := newResState(mk(resource.ModelHardBudget, 19, 1, 0, 0, 1), origin)
		s.observe(503, 0, origin)
		if s.effective() != 9 {
			t.Fatalf("eff=%d", s.effective())
		}
		ev := s.observe(200, 0, origin)
		assertKinds(t, ev, "cpu", []string{"aimd-increase"})
		if s.effective() != 10 {
			t.Fatalf("eff=%d", s.effective())
		}
		z := newResState(mk(resource.ModelLease, 1, 0, 0, 0, 1), origin)
		z.observe(429, 0, origin)
		if z.effective() != 0 || z.avail(origin) != 0 {
			t.Fatalf("eff=%d avail=%d", z.effective(), z.avail(origin))
		}
		ev = z.observe(200, 0, origin)
		assertKinds(t, ev, "cpu", []string{"aimd-increase"})
		if z.effective() != 1 || z.avail(origin) != 1 {
			t.Fatalf("recovered eff=%d avail=%d", z.effective(), z.avail(origin))
		}
	})
}

func TestRetryAfter(t *testing.T) {
	origin := epoch()
	s := newResState(mk(resource.ModelLease, 100, 10, 0, 0, 1), origin)
	if !s.take(4, origin) {
		t.Fatal("take")
	}
	ev := s.observe(429, 5*time.Second, origin)
	assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
	if s.effective() != 50 {
		t.Fatalf("eff=%d", s.effective())
	}
	if s.avail(origin) != 0 || s.avail(origin.Add(5*time.Second-time.Nanosecond)) != 0 {
		t.Fatal("avail during block")
	}
	got, ok := s.nextTimer()
	if !ok || !got.Equal(origin.Add(5*time.Second)) {
		t.Fatalf("timer=%s ok=%v", got, ok)
	}
	ev = s.advance(origin.Add(5*time.Second - time.Nanosecond))
	assertKinds(t, ev, "cpu", nil)
	s.giveBack(4, 0, 0, origin.Add(time.Second))
	if s.avail(origin.Add(time.Second)) != 0 {
		t.Fatal("giveBack cleared the block")
	}
	now := origin.Add(5 * time.Second)
	ev = s.advance(now)
	assertKinds(t, ev, "cpu", []string{"blocked-expired"})
	if !ev[0].At.Equal(now) {
		t.Fatalf("at %s", ev[0].At)
	}
	if s.advance(now) != nil {
		t.Fatal("second blocked-expired")
	}
	if s.avail(now) != 50 {
		t.Fatalf("avail=%d", s.avail(now))
	}

	t.Run("replace-and-success-keeps-block", func(t *testing.T) {
		s := newResState(mk(resource.ModelLease, 100, 10, 0, 0, 1), origin)
		s.observe(429, 10*time.Second, origin)
		ev := s.observe(429, 2*time.Second, origin.Add(time.Second))
		assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
		if s.effective() != 25 {
			t.Fatalf("eff=%d", s.effective())
		}
		got, ok := s.nextTimer()
		if !ok || !got.Equal(origin.Add(3*time.Second)) {
			t.Fatalf("timer=%s", got)
		}
		ev = s.observe(200, 0, origin.Add(2*time.Second))
		assertKinds(t, ev, "cpu", []string{"aimd-increase"})
		if s.effective() != 30 || s.avail(origin.Add(2*time.Second)) != 0 {
			t.Fatalf("eff=%d avail=%d", s.effective(), s.avail(origin.Add(2*time.Second)))
		}
		if s.advance(origin.Add(3*time.Second-time.Nanosecond)) != nil {
			t.Fatal("early expiry")
		}
		ev = s.advance(origin.Add(3 * time.Second))
		assertKinds(t, ev, "cpu", []string{"blocked-expired"})
		if s.avail(origin.Add(3*time.Second)) != 30 {
			t.Fatalf("avail=%d", s.avail(origin.Add(3*time.Second)))
		}
		if s.advance(origin.Add(10*time.Second)) != nil {
			t.Fatal("second expiry")
		}
	})

	t.Run("no-retry", func(t *testing.T) {
		s := newResState(mk(resource.ModelHardBudget, 100, 1, 0, 0, 1), origin)
		ev := s.observe(503, 0, origin)
		assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
		if s.effective() != 50 || s.avail(origin) != 50 {
			t.Fatalf("eff=%d avail=%d", s.effective(), s.avail(origin))
		}
		if _, ok := s.nextTimer(); ok {
			t.Fatal("timer without retry-after")
		}
	})
}

func TestNextTimer(t *testing.T) {
	origin := epoch()
	s := newResState(mk(resource.ModelLease, 10, 1, 0, 0, 1), origin)
	if _, ok := s.nextTimer(); ok {
		t.Fatal("lease")
	}
	s = newResState(mk(resource.ModelGCRA, 10, 1, time.Second, 0, 1), origin)
	if _, ok := s.nextTimer(); ok {
		t.Fatal("gcra")
	}
	ev := s.observe(429, 2*time.Second, origin)
	assertKinds(t, ev, "cpu", []string{"aimd-decrease"})
	got, ok := s.nextTimer()
	if !ok || !got.Equal(origin.Add(2*time.Second)) {
		t.Fatalf("gcra block timer=%s", got)
	}

	s = newResState(mk(resource.ModelWindowBucket, 100, 1, time.Minute, 0, 1), origin)
	got, ok = s.nextTimer()
	if !ok || !got.Equal(origin.Add(time.Minute)) {
		t.Fatalf("window timer=%s", got)
	}
	if !s.take(3, origin) {
		t.Fatal("take")
	}
	s.observe(429, 5*time.Second, origin)
	got, ok = s.nextTimer()
	if !ok || !got.Equal(origin.Add(5*time.Second)) {
		t.Fatalf("earlier timer=%s", got)
	}
	ev = s.advance(origin.Add(5 * time.Second))
	assertKinds(t, ev, "cpu", []string{"blocked-expired"})
	if s.windowID() != 0 || s.avail(origin.Add(5*time.Second)) != 47 {
		t.Fatalf("id=%d avail=%d", s.windowID(), s.avail(origin.Add(5*time.Second)))
	}
	got, ok = s.nextTimer()
	if !ok || !got.Equal(origin.Add(time.Minute)) {
		t.Fatalf("after block timer=%s", got)
	}
	ev = s.advance(origin.Add(time.Minute))
	assertKinds(t, ev, "cpu", []string{"window-reset"})
	if s.windowID() != 1 || s.avail(origin.Add(time.Minute)) != 50 {
		t.Fatalf("id=%d avail=%d", s.windowID(), s.avail(origin.Add(time.Minute)))
	}
	got, ok = s.nextTimer()
	if !ok || !got.Equal(origin.Add(2*time.Minute)) {
		t.Fatalf("next window=%s", got)
	}

	t.Run("same-instant", func(t *testing.T) {
		s := newResState(mk(resource.ModelWindowBucket, 100, 1, 5*time.Second, 0, 1), origin)
		if !s.take(3, origin) {
			t.Fatal("take")
		}
		s.observe(503, 5*time.Second, origin)
		now := origin.Add(5 * time.Second)
		ev := s.advance(now)
		assertKinds(t, ev, "cpu", []string{"window-reset", "blocked-expired"})
		if !ev[0].At.Equal(now) || !ev[1].At.Equal(now) {
			t.Fatalf("times %s %s", ev[0].At, ev[1].At)
		}
		if s.windowID() != 1 || s.effective() != 50 || s.avail(now) != 50 {
			t.Fatalf("id=%d eff=%d avail=%d", s.windowID(), s.effective(), s.avail(now))
		}
	})

	s = newResState(mk(resource.ModelCircuit, 10, 1, 0, 10*time.Second, 1), origin)
	if _, ok := s.nextTimer(); ok {
		t.Fatal("closed circuit")
	}
	s.observe(500, 0, origin)
	got, ok = s.nextTimer()
	if !ok || !got.Equal(origin.Add(10*time.Second)) {
		t.Fatalf("open timer=%s", got)
	}
	ev = s.advance(origin.Add(10 * time.Second))
	assertKinds(t, ev, "cpu", []string{"half-open"})
	if _, ok := s.nextTimer(); ok {
		t.Fatal("half-open timer")
	}
}

func TestWakeHint(t *testing.T) {
	origin := epoch()
	type hint struct {
		at   time.Duration
		need int64
		want time.Duration
	}
	check := func(t *testing.T, s *resState, rows []hint) {
		t.Helper()
		var prev time.Time
		for i, row := range rows {
			now := origin.Add(row.at)
			got := s.wakeHint(row.need, now)
			want := origin.Add(row.want)
			if !got.Equal(want) {
				t.Fatalf("row %d at %s need %d: hint %s want %s", i, row.at, row.need, got, want)
			}
			if got.Before(now) {
				t.Fatalf("row %d hint %s before now %s", i, got, now)
			}
			if i > 0 && got.Before(prev) {
				t.Fatalf("row %d hint %s moved backwards from %s", i, got, prev)
			}
			prev = got
		}
	}

	t.Run("lease-ready", func(t *testing.T) {
		s := newResState(mk(resource.ModelLease, 10, 1, 0, 0, 1), origin)
		check(t, s, []hint{
			{at: 0, need: 1, want: 0},
			{at: time.Second, need: 1, want: time.Second},
			{at: 2 * time.Second, need: 1, want: 2 * time.Second},
		})
		if got := s.wakeHint(11, origin); !got.Equal(origin.Add(time.Second)) {
			t.Fatalf("impossible %s", got)
		}
	})

	t.Run("lease-drained", func(t *testing.T) {
		s := newResState(mk(resource.ModelLease, 10, 1, 0, 0, 1), origin)
		if !s.take(10, origin) {
			t.Fatal("take")
		}
		check(t, s, []hint{
			{at: 0, need: 1, want: time.Second},
			{at: time.Second, need: 1, want: 2 * time.Second},
			{at: 2 * time.Second, need: 1, want: 3 * time.Second},
		})
	})

	t.Run("hard-drained", func(t *testing.T) {
		s := newResState(mk(resource.ModelHardBudget, 10, 1, 0, 0, 1), origin)
		if !s.take(10, origin) {
			t.Fatal("take")
		}
		check(t, s, []hint{
			{at: 0, need: 1, want: time.Second},
			{at: time.Second, need: 4, want: 2 * time.Second},
		})
	})

	t.Run("window", func(t *testing.T) {
		s := newResState(mk(resource.ModelWindowBucket, 10, 1, time.Minute, 0, 1), origin)
		if s.wakeHint(1, origin) != origin && !s.wakeHint(1, origin).Equal(origin) {
			t.Fatal("ready window")
		}
		if !s.take(10, origin) {
			t.Fatal("take")
		}
		check(t, s, []hint{
			{at: 0, need: 1, want: time.Minute},
			{at: 10 * time.Second, need: 1, want: time.Minute},
			{at: time.Minute, need: 1, want: time.Minute},
			{at: time.Minute + time.Second, need: 1, want: time.Minute + time.Second},
		})
		if got := s.wakeHint(11, origin); !got.Equal(origin.Add(time.Second)) {
			t.Fatalf("need above eff %s", got)
		}
	})

	t.Run("gcra", func(t *testing.T) {
		s := newResState(mk(resource.ModelGCRA, 100, 1, 10*time.Second, 0, 1), origin)
		if !s.take(100, origin) {
			t.Fatal("drain")
		}
		check(t, s, []hint{
			{at: 0, need: 30, want: 3 * time.Second},
			{at: time.Second, need: 30, want: 3 * time.Second},
			{at: 3 * time.Second, need: 30, want: 3 * time.Second},
			{at: 4 * time.Second, need: 30, want: 4 * time.Second},
		})
		got := s.wakeHint(101, origin.Add(4*time.Second))
		if !got.Equal(origin.Add(5 * time.Second)) {
			t.Fatalf("impossible gcra %s", got)
		}
	})

	t.Run("circuit", func(t *testing.T) {
		s := newResState(mk(resource.ModelCircuit, 80, 1, 0, 10*time.Second, 1), origin)
		if !s.wakeHint(1, origin).Equal(origin) {
			t.Fatal("closed hint")
		}
		s.observe(500, 0, origin)
		check(t, s, []hint{
			{at: 0, need: 1, want: 10 * time.Second},
			{at: 4 * time.Second, need: 1, want: 10 * time.Second},
			{at: 10 * time.Second, need: 1, want: 10 * time.Second},
			{at: 11 * time.Second, need: 1, want: 11 * time.Second},
		})
		s.advance(origin.Add(11 * time.Second))
		now := origin.Add(11 * time.Second)
		if !s.wakeHint(1, now).Equal(now) {
			t.Fatal("free half-open")
		}
		if !s.take(1, now) {
			t.Fatal("probe")
		}
		if !s.wakeHint(1, now).Equal(now.Add(time.Second)) {
			t.Fatal("probe in flight")
		}
	})

	t.Run("blocked", func(t *testing.T) {
		s := newResState(mk(resource.ModelLease, 100, 1, 0, 0, 1), origin)
		s.observe(429, 5*time.Second, origin)
		check(t, s, []hint{
			{at: 0, need: 1, want: 5 * time.Second},
			{at: 2 * time.Second, need: 1, want: 5 * time.Second},
			{at: 5 * time.Second, need: 1, want: 5 * time.Second},
			{at: 5 * time.Second, need: 1000, want: 6 * time.Second},
		})
		if got := s.wakeHint(51, origin); !got.Equal(origin.Add(time.Second)) {
			t.Fatalf("blocked but impossible %s", got)
		}
	})
}
