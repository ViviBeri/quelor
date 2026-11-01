package resource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
)

func lease(id string, cap int64) Manifest {
	return Manifest{ID: id, CapacityModel: ModelLease, Capacity: cap}
}

func TestRegistryRegister(t *testing.T) {
	reg := NewRegistry(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	in := lease("a", 4)
	if err := reg.Register(in, nil); err != nil {
		t.Fatal(err)
	}
	if in.MinCapacity != 0 || in.UnknownPolicy != "" {
		t.Fatalf("register mutated caller %+v", in)
	}
	if !reg.Has("a") || reg.Has("missing") {
		t.Fatalf("has a=%v missing=%v", reg.Has("a"), reg.Has("missing"))
	}
	got, ok := reg.Manifest("a")
	if !ok {
		t.Fatal("missing manifest")
	}
	if got.Capacity != 4 || got.MinCapacity != 1 || got.FailureThreshold != 3 || got.UnknownPolicy != UnknownDeferShort || got.OpenFor != Duration(30*time.Second) {
		t.Fatalf("stored %+v", got)
	}
	err := reg.Register(lease("a", 9), NewStaticReader(quelor.Reading{Known: true, Available: 1}))
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate err %v", err)
	}
	got, _ = reg.Manifest("a")
	if got.Capacity != 4 {
		t.Fatalf("replaced capacity %d", got.Capacity)
	}
	if err := reg.Register(Manifest{}, nil); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("invalid err %v", err)
	}
	if reg.Has("") {
		t.Fatal("stored empty id")
	}
	bad := Manifest{ID: "spend", CapacityModel: ModelHardBudget, Capacity: 1, UnknownPolicy: UnknownHoldCursor}
	if err := reg.Register(bad, nil); !errors.Is(err, ErrFailOpenSpend) {
		t.Fatalf("fail-open err %v", err)
	}
	if reg.Has("spend") {
		t.Fatal("stored fail-open manifest")
	}
	if err := reg.Register(lease("c", 1), nil); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(lease("b", 1), nil); err != nil {
		t.Fatal(err)
	}
	ids := reg.IDs()
	want := []string{"a", "b", "c"}
	if !sameIDs(ids, want) {
		t.Fatalf("ids %v", ids)
	}
	if _, ok := reg.Manifest("nope"); ok {
		t.Fatal("unexpected manifest")
	}
}

func TestRegistryRead(t *testing.T) {
	errProbe := errors.New("probe down")
	tests := []struct {
		name      string
		age       time.Duration
		ttl       time.Duration
		avail     int64
		known     bool
		markStale bool
		readErr   bool
		nilReader bool
		wantAvail int64
		wantKnown bool
		wantStale bool
		wantAge   int64
		wantCap   int64
	}{
		{name: "fresh", age: 9 * time.Second, ttl: 10 * time.Second, avail: 7, known: true, wantAvail: 7, wantKnown: true, wantStale: false, wantAge: 9000, wantCap: 42},
		{name: "exact ttl boundary fresh", age: 10 * time.Second, ttl: 10 * time.Second, avail: 7, known: true, wantAvail: 7, wantKnown: true, wantStale: false, wantAge: 10000, wantCap: 42},
		{name: "ttl plus one nanosecond stale", age: 10*time.Second + time.Nanosecond, ttl: 10 * time.Second, avail: 7, known: true, wantAvail: 0, wantKnown: true, wantStale: true, wantAge: 10000, wantCap: 42},
		{name: "ttl plus one millisecond stale", age: 10*time.Second + time.Millisecond, ttl: 10 * time.Second, avail: 7, known: true, wantAvail: 0, wantKnown: true, wantStale: true, wantAge: 10001, wantCap: 42},
		{name: "ttl disabled", age: time.Hour, ttl: 0, avail: 7, known: true, wantAvail: 7, wantKnown: true, wantStale: false, wantAge: 3_600_000, wantCap: 42},
		{name: "fresh zero available", age: 0, ttl: time.Second, avail: 0, known: true, wantAvail: 0, wantKnown: true, wantStale: false, wantAge: 0, wantCap: 42},
		{name: "probe marked stale", age: 0, ttl: 10 * time.Second, avail: 7, known: true, markStale: true, wantAvail: 0, wantKnown: true, wantStale: true, wantAge: 0, wantCap: 42},
		{name: "reader error", age: 0, ttl: 0, avail: 100, known: true, readErr: true, wantAvail: 0, wantKnown: false, wantStale: false, wantAge: 0, wantCap: 42},
		{name: "unknown reading", age: 0, ttl: 0, avail: 100, known: false, wantAvail: 0, wantKnown: false, wantStale: false, wantAge: 0, wantCap: 42},
		{name: "nil reader", nilReader: true, wantAvail: 0, wantKnown: false, wantStale: false, wantAge: 0, wantCap: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
			reg := NewRegistry(func() time.Time { return now })
			var r Reader
			if !tc.nilReader {
				r = FuncReader(func(ctx context.Context) (quelor.Reading, error) {
					var err error
					if tc.readErr {
						err = errProbe
					}
					return quelor.Reading{
						Available:  tc.avail,
						Capacity:   42,
						Known:      tc.known,
						Stale:      tc.markStale,
						ObservedAt: now.Add(-tc.age),
					}, err
				})
			}
			m := Manifest{
				ID:            "res",
				CapacityModel: ModelLease,
				Capacity:      42,
				FreshnessTTL:  Duration(tc.ttl),
			}
			if err := reg.Register(m, r); err != nil {
				t.Fatalf("register: %v", err)
			}
			got, err := reg.Read(context.Background(), "res")
			switch {
			case tc.nilReader:
				if !errors.Is(err, ErrNoReader) {
					t.Fatalf("err = %v", err)
				}
			case tc.readErr:
				if !errors.Is(err, errProbe) {
					t.Fatalf("err = %v", err)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v", err)
				}
			}
			if got.Resource != "res" {
				t.Fatalf("resource %q", got.Resource)
			}
			if got.Available != tc.wantAvail || got.Known != tc.wantKnown || got.Stale != tc.wantStale || got.AgeMS != tc.wantAge || got.Capacity != tc.wantCap {
				t.Fatalf("got %+v", got)
			}
			if Available(got) != tc.wantAvail {
				t.Fatalf("Available=%d want %d", Available(got), tc.wantAvail)
			}
			if tc.wantKnown && !tc.wantStale && !got.Usable() {
				t.Fatalf("expected usable %+v", got)
			}
			if (!tc.wantKnown || tc.wantStale) && got.Usable() {
				t.Fatalf("expected unusable %+v", got)
			}
		})
	}
}

func TestRegistryUnknownID(t *testing.T) {
	reg := NewRegistry(func() time.Time { return time.Unix(0, 0).UTC() })
	got, err := reg.Read(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if got.Known || got.Available != 0 || got.Resource != "missing" {
		t.Fatalf("reading %+v", got)
	}
}

func TestRegistryNilClock(t *testing.T) {
	reg := NewRegistry(nil)
	obs := time.Now().Add(time.Hour)
	err := reg.Register(Manifest{
		ID:            "live",
		CapacityModel: ModelLease,
		Capacity:      3,
		FreshnessTTL:  Duration(time.Second),
	}, NewStaticReader(quelor.Reading{
		Available:  3,
		Capacity:   8,
		Known:      true,
		ObservedAt: obs,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reg.Read(context.Background(), "live")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale || !got.Known || got.Available != 3 || got.Capacity != 8 {
		t.Fatalf("%+v", got)
	}
}
