package reference

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
)

func TestStoreLifecycle(t *testing.T) {
	s := NewStore()
	if rs, mode := s.Active(); rs != nil || mode != "" {
		t.Fatalf("active %+v %s", rs, mode)
	}
	if err := s.Revert(); err == nil || !strings.Contains(err.Error(), "previous") {
		t.Fatalf("revert %v", err)
	}
	a := mustParse(t, "id: aaa\nmode: enforce\nrules:\n  - id: a\n    when: \"true\"\n    verdict: DENY\n    reason: a\n")
	b := mustParse(t, "id: bbb\nmode: enforce\nrules:\n  - id: b\n    when: \"true\"\n    verdict: SHED\n    reason: b\n")
	c := mustParse(t, "id: ccc\nmode: log-only\nrules:\n  - id: c\n    when: \"true\"\n    verdict: ADMIT\n    reason: c\n")
	ha := s.Put(a)
	if s.Put(a) != ha || len(s.Hashes()) != 1 {
		t.Fatalf("idempotent hashes %+v", s.Hashes())
	}
	again := mustParse(t, "id: aaa\nmode: advise\nrules:\n  - id: a\n    when: \"true\"\n    verdict: DENY\n    reason: a\n")
	if s.Put(again) != ha {
		t.Fatal("same content hash changed")
	}
	got, ok := s.Get(ha)
	if !ok || got != a {
		t.Fatal("put replaced the first object")
	}
	hb := s.Put(b)
	hc := s.Put(c)
	if !reflect.DeepEqual(s.Hashes(), []string{ha, hb, hc}) {
		t.Fatalf("hashes %+v", s.Hashes())
	}
	alias := s.Hashes()
	alias[0] = "mutated"
	if s.Hashes()[0] != ha {
		t.Fatal("hashes alias")
	}

	if err := s.Activate(ha, decider.ModeEnforce); err != nil {
		t.Fatal(err)
	}
	if err := s.Revert(); err == nil {
		t.Fatal("revert with empty history")
	}
	active, mode := s.Active()
	if active != a || mode != decider.ModeEnforce {
		t.Fatalf("active %v %s", active, mode)
	}
	if err := s.Activate(hb, decider.ModeAdvise); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(hc, decider.ModeLogOnly); err != nil {
		t.Fatal(err)
	}
	active, mode = s.Active()
	if active != c || mode != decider.ModeLogOnly {
		t.Fatalf("active C %v %s", active, mode)
	}
	if err := s.Revert(); err != nil {
		t.Fatal(err)
	}
	active, mode = s.Active()
	if active != b || mode != decider.ModeAdvise {
		t.Fatalf("revert B %v %s", active, mode)
	}
	if err := s.Revert(); err != nil {
		t.Fatal(err)
	}
	active, mode = s.Active()
	if active != a || mode != decider.ModeEnforce {
		t.Fatalf("revert A %v %s", active, mode)
	}
	if err := s.Revert(); err == nil {
		t.Fatal("third revert")
	}
	active, mode = s.Active()
	if active != a || mode != decider.ModeEnforce {
		t.Fatalf("stayed %v %s", active, mode)
	}
	for _, h := range []string{ha, hb, hc} {
		if _, ok := s.Get(h); !ok {
			t.Fatalf("deleted %s", h)
		}
	}
	if !reflect.DeepEqual(s.Hashes(), []string{ha, hb, hc}) {
		t.Fatalf("hashes after revert %+v", s.Hashes())
	}
	if a.Mode != decider.ModeEnforce || b.Mode != decider.ModeEnforce {
		t.Fatal("activate mutated yaml mode")
	}
}

func TestStoreDeciderFollowsActive(t *testing.T) {
	s := NewStore()
	view := s.Decider()
	resp, err := view.Decide(context.Background(), baseReq())
	if err != nil {
		t.Fatal(err)
	}
	assertIdentity(t, resp, "trace-1")
	if resp.Status != decider.StatusUnavailable || resp.Enforced() {
		t.Fatalf("status %s", resp.Status)
	}
	if !reflect.DeepEqual(resp.Abstains, []string{"no-active-ruleset"}) {
		t.Fatalf("abstains %+v", resp.Abstains)
	}

	a := mustParse(t, "id: aaa\nmode: enforce\nrules:\n  - id: deny\n    when: \"true\"\n    verdict: DENY\n    reason: no\n")
	b := mustParse(t, "id: bbb\nmode: enforce\nrules:\n  - id: admit\n    when: \"true\"\n    verdict: ADMIT\n    reason: yes\n")
	ha := s.Put(a)
	hb := s.Put(b)
	if err := s.Activate(ha, decider.ModeLogOnly); err != nil {
		t.Fatal(err)
	}
	resp = mustDecide(t, view, baseReq())
	if resp.RulesetID != "aaa" || resp.RulesetHash != a.Hash() || resp.Mode != decider.ModeLogOnly || resp.Enforced() {
		t.Fatalf("id %s mode %s enforced %v", resp.RulesetID, resp.Mode, resp.Enforced())
	}
	if resp.Verdict.Kind != quelor.Deny || resp.Verdict.Reason != "no" {
		t.Fatalf("%+v", resp.Verdict)
	}
	if a.Mode != decider.ModeEnforce {
		t.Fatal("yaml mode overwritten")
	}

	if err := s.Activate(hb, decider.ModeAdvise); err != nil {
		t.Fatal(err)
	}
	resp = mustDecide(t, view, baseReq())
	if resp.RulesetID != "bbb" || resp.Mode != decider.ModeAdvise || resp.Enforced() || resp.Verdict.Kind != quelor.Admit {
		t.Fatalf("id %s mode %s %+v", resp.RulesetID, resp.Mode, resp.Verdict)
	}
	if err := s.Revert(); err != nil {
		t.Fatal(err)
	}
	resp = mustDecide(t, view, baseReq())
	if resp.RulesetID != "aaa" || resp.Mode != decider.ModeLogOnly || resp.Verdict.Kind != quelor.Deny {
		t.Fatalf("id %s mode %s %+v", resp.RulesetID, resp.Mode, resp.Verdict)
	}
	if _, ok := s.Get(hb); !ok {
		t.Fatal("reverted ruleset deleted")
	}
}

func TestStoreErrors(t *testing.T) {
	s := NewStore()
	rs := mustParse(t, "id: aaa\nrules: []\n")
	h := s.Put(rs)
	if s.Put(nil) != "" {
		t.Fatal("nil put")
	}
	tests := []struct {
		name string
		hash string
		mode decider.Mode
		want string
	}{
		{name: "missing", hash: "nope", mode: decider.ModeEnforce, want: "unknown"},
		{name: "empty mode", hash: h, mode: "", want: "invalid mode"},
		{name: "bad mode", hash: h, mode: decider.Mode("ENFORCE"), want: "invalid mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.Activate(tt.hash, tt.mode)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
	if _, mode := s.Active(); mode != "" {
		t.Fatalf("mode %s", mode)
	}
}

func TestStoreConcurrent(t *testing.T) {
	s := NewStore()
	view := s.Decider()
	const n = 24
	hashes := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			y := fmt.Sprintf("id: rs-%d\nmode: enforce\nrules:\n  - id: r\n    when: \"unit_id == '%d'\"\n    verdict: DENY\n    reason: n\n", i, i)
			rs, err := Parse([]byte(y))
			if err != nil {
				t.Errorf("parse: %v", err)
				return
			}
			<-start
			h := s.Put(rs)
			hashes[i] = h
			if _, ok := s.Get(h); !ok {
				t.Errorf("missing %s", h)
			}
			if err := s.Activate(h, decider.ModeEnforce); err != nil {
				t.Errorf("activate: %v", err)
			}
			if _, err := view.Decide(context.Background(), baseReq()); err != nil {
				t.Errorf("decide: %v", err)
			}
			_ = s.Hashes()
			_ = s.Revert()
			if _, ok := s.Get(h); !ok {
				t.Errorf("deleted %s", h)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	got := s.Hashes()
	if len(got) != n {
		t.Fatalf("hashes %d", len(got))
	}
	have := map[string]bool{}
	for _, h := range got {
		if have[h] {
			t.Fatalf("duplicate %s", h)
		}
		have[h] = true
	}
	for i, h := range hashes {
		if !have[h] {
			t.Errorf("missing hash %d", i)
		}
		if _, ok := s.Get(h); !ok {
			t.Errorf("get %d", i)
		}
	}
}
