package admit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setNow(t *testing.T, tm time.Time) {
	t.Helper()
	nowMu.Lock()
	prev := nowFn
	nowFn = func() time.Time { return tm }
	nowMu.Unlock()
	t.Cleanup(func() {
		nowMu.Lock()
		nowFn = prev
		nowMu.Unlock()
	})
}

type decideRow struct {
	name      string
	in        Reading
	err       error
	need      int64
	optional  bool
	verdict   string
	exit      int
	remaining int64
	known     bool
	reason    []string
}

func dr(name string, in Reading, err error, need int64, optional bool, verdict string, exit int, remaining int64, known bool, reason ...string) decideRow {
	return decideRow{name: name, in: in, err: err, need: need, optional: optional, verdict: verdict, exit: exit, remaining: remaining, known: known, reason: reason}
}

func wantLine(verdict, resource string, need, remaining int64, reset time.Time) string {
	rs := ""
	if !reset.IsZero() {
		rs = reset.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s %s need=%d remaining=%d reset=%s", verdict, resource, need, remaining, rs)
}

func TestDecide(t *testing.T) {
	reset := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	setNow(t, reset.Add(-120*time.Second))
	base := Reading{Resource: "github_api", Limit: 5000, Reset: reset}

	plenty := base
	plenty.Remaining = 4950
	short := base
	short.Remaining = 10
	exact := base
	exact.Remaining = 50
	// A huge remaining must still be ignored when the reading is unknown.
	unknown := base
	unknown.Remaining = 99999

	tests := []decideRow{
		dr("admit", plenty, nil, 50, false, "admit", 0, 4950, true, "remaining covers need"),
		dr("defer optional", short, nil, 50, true, "defer", 42, 10, true, "2026-10-31T15:00:00Z", "120 seconds until reset"),
		dr("degraded not optional", short, nil, 50, false, "degraded", 43, 10, true, "demand is required"),
		dr("read error", unknown, errors.New("boom"), 50, false, "degraded", 43, 0, false, "boom", "not treated as available"),
		dr("empty token", unknown, ErrMissingToken, 50, true, "degraded", 43, 0, false, "missing token", "not treated as available"),
		dr("exact boundary remaining==need admits", exact, nil, 50, false, "admit", 0, 50, true, "remaining covers need"),
		dr("optional boundary still admits", exact, nil, 50, true, "admit", 0, 50, true, "remaining covers need"),
		dr("wrapped empty token", unknown, fmt.Errorf("wrap: %w", ErrMissingToken), 50, false, "degraded", 43, 0, false, "missing token", "not treated as available"),
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "defer reset in the past" {
				setNow(t, reset.Add(30*time.Second))
			}
			got := Decide(tt.in, tt.err, tt.need, tt.optional)
			if got.Verdict != tt.verdict || got.Exit != tt.exit {
				t.Fatalf("verdict/exit = %s/%d, want %s/%d (%s)", got.Verdict, got.Exit, tt.verdict, tt.exit, got.Reason)
			}
			if got.Resource != "github_api" || got.Need != tt.need || got.Remaining != tt.remaining || got.known != tt.known {
				t.Fatalf("got resource=%s need=%d remaining=%d known=%v", got.Resource, got.Need, got.Remaining, got.known)
			}
			if tt.known {
				if !got.Reset.Equal(reset) {
					t.Fatalf("reset %s", got.Reset)
				}
			} else if !got.Reset.IsZero() {
				t.Fatalf("unknown reading copied reset %s", got.Reset)
			}
			for _, part := range tt.reason {
				if !strings.Contains(got.Reason, part) {
					t.Fatalf("reason %q missing %q", got.Reason, part)
				}
			}
			wantReset := time.Time{}
			if tt.known {
				wantReset = reset
			}
			if got.Line() != wantLine(tt.verdict, "github_api", tt.need, tt.remaining, wantReset) {
				t.Fatalf("line %q", got.Line())
			}
			if strings.Contains(got.Line(), "99999") {
				t.Fatalf("line treated unknown remaining as capacity: %s", got.Line())
			}
		})
	}

	t.Run("defer reset in the past", func(t *testing.T) {
		setNow(t, reset.Add(30*time.Second))
		got := Decide(short, nil, 50, true)
		if got.Verdict != "defer" || got.Exit != 42 {
			t.Fatalf("got %s/%d", got.Verdict, got.Exit)
		}
		if !strings.Contains(got.Reason, "2026-10-31T15:00:00Z") || !strings.Contains(got.Reason, "0 seconds until reset") {
			t.Fatalf("reason %q", got.Reason)
		}
	})

	const example = "admit github_api need=50 remaining=4950 reset=2026-10-31T15:00:00Z"
	if got := Decide(plenty, nil, 50, false).Line(); got != example {
		t.Fatalf("example line %q", got)
	}
}

func TestWriteGitHubOutput(t *testing.T) {
	reset := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	admitRes := Decide(Reading{Resource: "github_api", Remaining: 4950, Reset: reset}, nil, 50, false)
	zero := Decide(Reading{Resource: "github_api", Remaining: 0, Reset: reset}, nil, 1, false)
	unknown := Decide(Reading{Resource: "github_api", Remaining: 99999, Reset: reset}, ErrMissingToken, 50, false)

	if err := admitRes.WriteGitHubOutput(""); err != nil {
		t.Fatal(err)
	}
	if err := admitRes.WriteGitHubOutput(t.TempDir()); err == nil {
		t.Fatal("expected error for directory path")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "out")
	if err := os.WriteFile(path, []byte("existing=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := admitRes.WriteGitHubOutput(path); err != nil {
		t.Fatal(err)
	}
	if err := zero.WriteGitHubOutput(path); err != nil {
		t.Fatal(err)
	}
	if err := unknown.WriteGitHubOutput(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "existing=1\nverdict=admit\nremaining=4950\nverdict=degraded\nremaining=0\nverdict=degraded\nremaining=\n"
	if string(body) != want {
		t.Fatalf("output:\n%s\nwant:\n%s", body, want)
	}
}

func TestResolveBaseURL(t *testing.T) {
	t.Setenv("GITHUB_API_URL", "")
	if got := (&Client{}).resolveBaseURL(); got != "https://api.github.com" {
		t.Fatalf("default %s", got)
	}
	t.Setenv("GITHUB_API_URL", "https://ghe.example/api/v3/")
	if got := (&Client{}).resolveBaseURL(); got != "https://ghe.example/api/v3" {
		t.Fatalf("env %s", got)
	}
	c := &Client{BaseURL: " http://127.0.0.1:9/ "}
	if got := c.resolveBaseURL(); got != "http://127.0.0.1:9" {
		t.Fatalf("field %s", got)
	}
}

func TestHTTPClientDefault(t *testing.T) {
	c := &Client{}
	hc := c.httpClient()
	if hc == nil || hc.Timeout != 10*time.Second {
		t.Fatalf("timeout %v", hc)
	}
	custom := &http.Client{Timeout: time.Second}
	c.HTTP = custom
	if c.httpClient() != custom {
		t.Fatal("custom client not used")
	}
}

func TestClientRead(t *testing.T) {
	token := "ghs_SUPERSECRET"
	reset := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{"resources":{
		"core":{"limit":5000,"remaining":4950,"reset":%d,"used":50},
		"search":{"limit":30,"remaining":12,"reset":%d,"used":18},
		"graphql":{"limit":5000,"remaining":4000,"reset":%d,"used":1000},
		"code_search":{"limit":10,"remaining":7,"reset":%d,"used":3}
	}}`, reset.Unix(), reset.Unix(), reset.Unix(), reset.Unix())

	var mu sync.Mutex
	var hits int
	var bad string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		switch {
		case r.Method != http.MethodGet:
			bad = "method " + r.Method
		case r.URL.Path != "/rate_limit":
			bad = "path " + r.URL.Path
		case r.Header.Get("Authorization") != "Bearer "+token:
			bad = "auth " + r.Header.Get("Authorization")
		case r.Header.Get("Accept") != "application/vnd.github+json":
			bad = "accept " + r.Header.Get("Accept")
		case r.Header.Get("X-GitHub-Api-Version") != "2022-11-28":
			bad = "version " + r.Header.Get("X-GitHub-Api-Version")
		case r.Header.Get("User-Agent") != "quelor-admit":
			bad = "ua " + r.Header.Get("User-Agent")
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := &Client{BaseURL: srv.URL + "/", Token: "  " + token + "  ", HTTP: srv.Client()}
	cases := []struct {
		resource  string
		limit     int64
		remaining int64
	}{
		{"github_api", 5000, 4950},
		{"github_search", 30, 12},
		{"github_graphql", 5000, 4000},
		{"github_code_search", 10, 7},
	}
	for _, tc := range cases {
		t.Run(tc.resource, func(t *testing.T) {
			got, err := c.Read(context.Background(), tc.resource)
			if err != nil {
				t.Fatal(err)
			}
			if got.Resource != tc.resource || got.Limit != tc.limit || got.Remaining != tc.remaining {
				t.Fatalf("got %+v", got)
			}
			if got.Reset.UTC().Format(time.RFC3339) != "2026-10-31T15:00:00Z" {
				t.Fatalf("reset %s", got.Reset)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if bad != "" {
		t.Fatal(bad)
	}
	if hits != len(cases) {
		t.Fatalf("hits %d", hits)
	}
}

func TestClientReadErrors(t *testing.T) {
	token := "ghs_SUPERSECRET"

	t.Run("non-200 does not leak token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"no","token":"` + token + `","authorization":"Bearer ` + token + `"}`))
		}))
		t.Cleanup(srv.Close)
		c := &Client{BaseURL: srv.URL, Token: token, HTTP: srv.Client()}
		_, err := c.Read(context.Background(), "github_api")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "Bearer") {
			t.Fatalf("token leaked: %s", err)
		}
		if !strings.Contains(err.Error(), "http 403") {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resources":`))
		}))
		t.Cleanup(srv.Close)
		c := &Client{BaseURL: srv.URL, Token: token, HTTP: srv.Client()}
		_, err := c.Read(context.Background(), "github_api")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("token leaked: %s", err)
		}
	})

	t.Run("resource absent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resources":{"core":{"limit":1,"remaining":1,"reset":1,"used":0}}}`))
		}))
		t.Cleanup(srv.Close)
		c := &Client{BaseURL: srv.URL, Token: token, HTTP: srv.Client()}
		_, err := c.Read(context.Background(), "github_search")
		if err == nil || !strings.Contains(err.Error(), "absent") {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("unknown resource does not call server", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		c := &Client{BaseURL: srv.URL, Token: token, HTTP: srv.Client()}
		_, err := c.Read(context.Background(), "not-a-resource")
		if err == nil || !strings.Contains(err.Error(), "unknown resource") {
			t.Fatalf("err %v", err)
		}
		if hits != 0 {
			t.Fatalf("hits %d", hits)
		}
	})

	t.Run("empty token", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
		}))
		t.Cleanup(srv.Close)
		for _, tok := range []string{"", "   "} {
			c := &Client{BaseURL: srv.URL, Token: tok, HTTP: srv.Client()}
			_, err := c.Read(context.Background(), "github_api")
			if !errors.Is(err, ErrMissingToken) {
				t.Fatalf("token %q err %v", tok, err)
			}
		}
		if hits != 0 {
			t.Fatalf("hits %d", hits)
		}
	})

	t.Run("transport error scrubs token", func(t *testing.T) {
		c := &Client{
			BaseURL: "http://127.0.0.1:9",
			Token:   token,
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("auth %s", r.Header.Get("Authorization"))
				}
				if r.URL.Path != "/rate_limit" {
					t.Errorf("path %s", r.URL.Path)
				}
				return nil, fmt.Errorf("dial %s failed", token)
			})},
		}
		_, err := c.Read(context.Background(), "github_graphql")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("token leaked: %s", err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := &Client{
			BaseURL: "https://example.invalid",
			Token:   token,
			HTTP:    &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New("should not dial") })},
		}
		_, err := c.Read(ctx, "github_api")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("token leaked: %s", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
