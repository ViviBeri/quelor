package decider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
)

type shadowObs struct {
	unit string
	resp Response
	err  error
}

func TestEffectiveShadowTimeout(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "zero", in: 0, want: defaultShadowTimeout},
		{name: "negative", in: -time.Millisecond, want: defaultShadowTimeout},
		{name: "set", in: 12 * time.Millisecond, want: 12 * time.Millisecond},
	}
	if defaultShadowTimeout != 50*time.Millisecond {
		t.Fatalf("default %s", defaultShadowTimeout)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveShadowTimeout(tc.in); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestShadowPrimaryUnaffected(t *testing.T) {
	verdict := quelor.Verdict{Kind: quelor.Admit, Reason: "ok", Stamps: []string{"live"}}
	req := Request{Op: OpAcquire, Envelope: quelor.Envelope{UnitID: "u-1"}, Meta: CallMeta{TraceID: "tr"}}
	primary := Static(verdict, "primary")
	want, wantErr := primary.Decide(context.Background(), req)
	if wantErr != nil {
		t.Fatal(wantErr)
	}

	errShadow := Func(func(context.Context, Request) (Response, error) {
		var r Response
		r.Status = StatusAnswered
		r.DeciderID = "err-shadow"
		r.Verdict.Kind = quelor.Deny
		r.Verdict.Reason = "should-not-merge"
		return r, errors.New("boom")
	})
	panicShadow := Func(func(context.Context, Request) (Response, error) {
		panic("shadow-boom")
	})
	slowShadow := Func(func(ctx context.Context, _ Request) (Response, error) {
		<-ctx.Done()
		var r Response
		r.Status = StatusAnswered
		r.DeciderID = "slow-shadow"
		r.Verdict.Kind = quelor.Shed
		return r, ctx.Err()
	})

	cases := []struct {
		name    string
		shadows []Decider
		timeout time.Duration
		hooks   bool
	}{
		{name: "error", shadows: []Decider{errShadow}, timeout: 20 * time.Millisecond, hooks: true},
		{name: "panic", shadows: []Decider{panicShadow}, timeout: 20 * time.Millisecond, hooks: true},
		{name: "slow", shadows: []Decider{slowShadow}, timeout: 25 * time.Millisecond, hooks: true},
		{name: "all", shadows: []Decider{errShadow, panicShadow, slowShadow}, timeout: 25 * time.Millisecond, hooks: true},
		{name: "nil-hook", shadows: []Decider{errShadow, panicShadow}, timeout: 20 * time.Millisecond, hooks: false},
		{name: "nil-entries", shadows: []Decider{nil, nil}, timeout: 20 * time.Millisecond, hooks: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var got []shadowObs
			s := &Slots{Primary: primary, Shadows: tc.shadows, ShadowTimeout: tc.timeout}
			if tc.hooks {
				s.OnShadow = func(r Request, resp Response, err error) {
					mu.Lock()
					got = append(got, shadowObs{unit: r.Envelope.UnitID, resp: resp, err: err})
					mu.Unlock()
				}
			}
			start := time.Now()
			resp, err := s.Decide(context.Background(), req)
			elapsed := time.Since(start)
			if elapsed > 15*time.Millisecond {
				t.Fatalf("primary waited %s", elapsed)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resp, want) {
				t.Fatalf("primary changed\n got %#v\nwant %#v", resp, want)
			}
			s.Wait()
			if !tc.hooks {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			wantN := 0
			for _, sh := range tc.shadows {
				if sh != nil {
					wantN++
				}
			}
			if len(got) != wantN {
				t.Fatalf("OnShadow calls %d want %d", len(got), wantN)
			}
			for _, ob := range got {
				if ob.unit != "u-1" {
					t.Fatalf("unit %q", ob.unit)
				}
			}
			switch tc.name {
			case "error":
				if got[0].resp.Verdict.Kind != quelor.Deny || got[0].err == nil || got[0].err.Error() != "boom" {
					t.Fatalf("obs %#v %v", got[0].resp, got[0].err)
				}
				if resp.Verdict.Kind == quelor.Deny {
					t.Fatal("deny merged")
				}
			case "panic":
				if got[0].resp.Status != StatusUnavailable || got[0].err == nil || !errors.Is(got[0].err, ErrUnavailable) {
					t.Fatalf("obs %#v %v", got[0].resp, got[0].err)
				}
				if !strings.Contains(got[0].err.Error(), "shadow-boom") {
					t.Fatalf("panic text %v", got[0].err)
				}
			case "slow":
				if !errors.Is(got[0].err, context.DeadlineExceeded) {
					t.Fatalf("slow err %v", got[0].err)
				}
				if got[0].resp.Verdict.Kind != quelor.Shed {
					t.Fatalf("slow resp %#v", got[0].resp)
				}
			case "all":
				var sawBoom, sawPanic, sawSlow bool
				for _, ob := range got {
					switch {
					case ob.err != nil && ob.err.Error() == "boom":
						sawBoom = true
					case ob.err != nil && strings.Contains(ob.err.Error(), "shadow-boom"):
						sawPanic = true
					case errors.Is(ob.err, context.DeadlineExceeded):
						sawSlow = true
					}
				}
				if !sawBoom || !sawPanic || !sawSlow {
					t.Fatalf("missing observations %#v", got)
				}
			case "nil-entries":
				if len(got) != 0 {
					t.Fatalf("calls %#v", got)
				}
			}
		})
	}
}

func TestShadowEmptyStartsNoWait(t *testing.T) {
	called := false
	s := &Slots{
		Primary: Static(quelor.Verdict{Kind: quelor.Admit}, "p"),
		OnShadow: func(Request, Response, error) {
			called = true
		},
	}
	resp, err := s.Decide(context.Background(), Request{Op: OpAcquire, Envelope: quelor.Envelope{UnitID: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Wait()
	if time.Since(start) > 10*time.Millisecond {
		t.Fatal("Wait blocked with no shadows")
	}
	if called {
		t.Fatal("OnShadow called")
	}
	if resp.DeciderID != "p" || resp.Verdict.Kind != quelor.Admit {
		t.Fatalf("%#v", resp)
	}
}

func TestShadowPrimaryErrorPreserved(t *testing.T) {
	primary := Func(func(context.Context, Request) (Response, error) {
		return Unavailable("primary", "nope"), fmt.Errorf("%w: nope", ErrUnavailable)
	})
	req := Request{Op: OpAcquire, Envelope: quelor.Envelope{UnitID: "u"}}
	want, wantErr := primary.Decide(context.Background(), req)
	s := &Slots{
		Primary:       primary,
		ShadowTimeout: 15 * time.Millisecond,
		Shadows: []Decider{Func(func(context.Context, Request) (Response, error) {
			panic("shadow-boom")
		})},
		OnShadow: func(Request, Response, error) {},
	}
	got, err := s.Decide(context.Background(), req)
	s.Wait()
	if err == nil || err.Error() != wantErr.Error() || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestShadowNilPrimaryAndSlots(t *testing.T) {
	cases := []struct {
		name string
		s    *Slots
	}{
		{name: "nil-slots", s: nil},
		{name: "nil-primary", s: &Slots{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.s.Decide(context.Background(), Request{})
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err %v", err)
			}
			if resp.Status != StatusUnavailable {
				t.Fatalf("%#v", resp)
			}
			tc.s.Wait()
		})
	}
}

func TestShadowContextDetached(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	parent = context.WithValue(parent, key{}, "trace-val")

	var remain time.Duration
	var hasDeadline bool
	var value string
	sh := Func(func(ctx context.Context, _ Request) (Response, error) {
		value, _ = ctx.Value(key{}).(string)
		var dl time.Time
		dl, hasDeadline = ctx.Deadline()
		remain = time.Until(dl)
		var r Response
		r.Status = StatusAbstained
		r.DeciderID = "sh"
		return r, nil
	})
	s := &Slots{
		Primary:       Static(quelor.Verdict{Kind: quelor.Admit}, "p"),
		Shadows:       []Decider{sh},
		ShadowTimeout: time.Second,
	}
	if _, err := s.Decide(parent, Request{Op: OpAcquire}); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if value != "trace-val" {
		t.Fatalf("value %q", value)
	}
	if !hasDeadline || remain < 200*time.Millisecond {
		t.Fatalf("deadline ok=%v remain=%s", hasDeadline, remain)
	}
}

func TestShadowSurvivesParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var got error
	s := &Slots{
		Primary:       Static(quelor.Verdict{Kind: quelor.Admit}, "p"),
		ShadowTimeout: 20 * time.Millisecond,
		Shadows: []Decider{Func(func(ctx context.Context, _ Request) (Response, error) {
			<-ctx.Done()
			var r Response
			r.Status = StatusAbstained
			r.DeciderID = "sh"
			return r, ctx.Err()
		})},
		OnShadow: func(_ Request, _ Response, err error) {
			got = err
		},
	}
	resp, err := s.Decide(ctx, Request{Op: OpAcquire, Envelope: quelor.Envelope{UnitID: "u"}})
	cancel()
	if err != nil || resp.Verdict.Kind != quelor.Admit {
		t.Fatalf("primary %#v %v", resp, err)
	}
	s.Wait()
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("shadow err %v", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatal("shadow observed parent cancel")
	}
}

func TestShadowDefaultDeadline(t *testing.T) {
	var remain time.Duration
	var ok bool
	s := &Slots{
		Primary: Static(quelor.Verdict{Kind: quelor.Admit}, "p"),
		Shadows: []Decider{Func(func(ctx context.Context, _ Request) (Response, error) {
			var dl time.Time
			dl, ok = ctx.Deadline()
			remain = time.Until(dl)
			var r Response
			r.Status = StatusAbstained
			return r, nil
		})},
	}
	if _, err := s.Decide(context.Background(), Request{Op: OpAcquire}); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if !ok || remain <= 40*time.Millisecond || remain > 50*time.Millisecond+time.Millisecond {
		t.Fatalf("remain %s ok %v", remain, ok)
	}
}

func TestShadowConcurrentDecide(t *testing.T) {
	primary := Static(quelor.Verdict{Kind: quelor.Admit, Reason: "ok"}, "primary")
	var mu sync.Mutex
	n := 0
	s := &Slots{
		Primary:       primary,
		ShadowTimeout: 20 * time.Millisecond,
		Shadows: []Decider{Func(func(context.Context, Request) (Response, error) {
			var r Response
			r.Status = StatusAbstained
			r.DeciderID = "sh"
			return r, nil
		})},
	}
	s.OnShadow = func(Request, Response, error) {
		mu.Lock()
		n++
		mu.Unlock()
	}
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			resp, err := s.Decide(context.Background(), Request{Op: OpAcquire, Envelope: quelor.Envelope{UnitID: "u"}})
			if err != nil || resp.Verdict.Kind != quelor.Admit || resp.DeciderID != "primary" {
				errCh <- fmt.Errorf("%#v %v", resp, err)
				return
			}
			errCh <- nil
		}()
	}
	wg.Wait()
	s.Wait()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Fatalf("shadow calls %d", n)
	}
}

func TestShadowReuseAfterWait(t *testing.T) {
	var mu sync.Mutex
	n := 0
	s := &Slots{
		Primary:       Static(quelor.Verdict{Kind: quelor.Shed}, "p"),
		ShadowTimeout: 10 * time.Millisecond,
		Shadows: []Decider{Func(func(context.Context, Request) (Response, error) {
			return Response{Status: StatusAbstained, DeciderID: "sh"}, nil
		})},
		OnShadow: func(Request, Response, error) {
			mu.Lock()
			n++
			mu.Unlock()
		},
	}
	for i := 0; i < 2; i++ {
		resp, err := s.Decide(context.Background(), Request{Op: OpRenew, Envelope: quelor.Envelope{UnitID: "u"}})
		if err != nil || resp.Verdict.Kind != quelor.Shed {
			t.Fatalf("%#v %v", resp, err)
		}
		s.Wait()
	}
	if n != 2 {
		t.Fatalf("calls %d", n)
	}
}
