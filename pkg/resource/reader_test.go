package resource

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/coherera/quelor/pkg/quelor"
)

func TestStaticReaderSet(t *testing.T) {
	ctx := context.Background()
	s := NewStaticReader(quelor.Reading{Available: 1, Capacity: 2, Known: true})
	got, err := s.Read(ctx)
	if err != nil || got.Available != 1 || got.Capacity != 2 || !got.Known {
		t.Fatalf("got %+v err %v", got, err)
	}
	s.Set(quelor.Reading{Available: 5, Capacity: 6, Known: true})
	got, err = s.Read(ctx)
	if err != nil || got.Available != 5 || got.Capacity != 6 || !got.Known {
		t.Fatalf("after set %+v err %v", got, err)
	}
}

func TestStaticReaderConcurrentSet(t *testing.T) {
	s := NewStaticReader(quelor.Reading{Known: true, Available: 1})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int64) {
			defer wg.Done()
			s.Set(quelor.Reading{Known: true, Available: n})
			if _, err := s.Read(context.Background()); err != nil {
				t.Errorf("read: %v", err)
			}
		}(int64(i + 1))
	}
	wg.Wait()
	s.Set(quelor.Reading{Known: true, Available: 99, Capacity: 100})
	got, err := s.Read(context.Background())
	if err != nil || got.Available != 99 || got.Capacity != 100 || !got.Known {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestFuncReader(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "v")
	f := FuncReader(func(ctx context.Context) (quelor.Reading, error) {
		if ctx.Value(ctxKey{}) != "v" {
			return quelor.Reading{}, errors.New("missing ctx")
		}
		return quelor.Reading{Known: true, Available: 3, Capacity: 3}, nil
	})
	got, err := f.Read(ctx)
	if err != nil || got.Available != 3 || !got.Known {
		t.Fatalf("got %+v err %v", got, err)
	}
	var nilFn FuncReader
	if _, err := nilFn.Read(context.Background()); err == nil {
		t.Fatal("expected nil func error")
	}
}

func TestAvailable(t *testing.T) {
	tests := []struct {
		name string
		in   quelor.Reading
		want int64
	}{
		{name: "usable", in: quelor.Reading{Available: 4, Known: true}, want: 4},
		{name: "unknown", in: quelor.Reading{Available: 4, Known: false}, want: 0},
		{name: "stale", in: quelor.Reading{Available: 4, Known: true, Stale: true}, want: 0},
		{name: "unknown and stale", in: quelor.Reading{Available: 4, Stale: true}, want: 0},
		{name: "usable zero", in: quelor.Reading{Available: 0, Known: true}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Available(tc.in); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
