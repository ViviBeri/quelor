package quelor

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPriorityRank(t *testing.T) {
	order := []Priority{PriorityOperatorDirective, PriorityReleaseCritical, PriorityChange, PriorityScheduled, PriorityMaintenance}
	for i, p := range order {
		if p.Rank() != i {
			t.Fatalf("%s rank %d want %d", p, p.Rank(), i)
		}
	}
	if Priority("bogus").Rank() != PriorityChange.Rank() {
		t.Fatal("unknown priority should rank as change")
	}
}

func TestReadingUsable(t *testing.T) {
	cases := []struct {
		r    Reading
		want bool
	}{
		{Reading{Known: true}, true},
		{Reading{Known: true, Stale: true}, false},
		{Reading{Known: false}, false},
		{Reading{}, false},
	}
	for i, c := range cases {
		if c.r.Usable() != c.want {
			t.Fatalf("case %d", i)
		}
	}
}

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	d := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := Envelope{UnitID: "u1", TenantID: "t", HolderID: "h", Demands: map[string]int64{"a": 2}, Deadline: &d, CoalescePolicy: CoalesceDropNew}
	b, _ := json.Marshal(in)
	var out Envelope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.UnitID != "u1" || out.Demands["a"] != 2 || !out.Deadline.Equal(d) || out.CoalescePolicy != CoalesceDropNew {
		t.Fatalf("roundtrip %+v", out)
	}
}
