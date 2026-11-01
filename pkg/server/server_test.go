package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/engine"
	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/resource"
	"github.com/coherera/quelor/pkg/server"
	"github.com/coherera/quelor/pkg/version"
	"gopkg.in/yaml.v3"
)

const (
	manifestSlots1 = "- id: slots\n  capacity_model: lease\n  capacity: 1\n"
	manifestSlots4 = "- id: slots\n  capacity_model: lease\n  capacity: 4\n"
)

func TestBackendSatisfied(t *testing.T) {
	var _ server.Backend = (*fakeBackend)(nil)
	var _ server.Backend = (*engine.Engine)(nil)
}

func TestRoutingAndBadJSON(t *testing.T) {
	h := server.New(&fakeBackend{}, server.Options{})
	tests := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodGet, "/healthz", "", 200},
		{http.MethodPost, "/healthz", "", 405},
		{http.MethodGet, "/metrics", "", 200},
		{http.MethodPost, "/metrics", "", 405},
		{http.MethodPost, "/v1/lease/acquire", "", 400},
		{http.MethodPost, "/v1/lease/acquire", "{", 400},
		{http.MethodPost, "/v1/lease/acquire", "[]", 400},
		{http.MethodGet, "/v1/lease/acquire", "", 405},
		{http.MethodPut, "/v1/lease/acquire", "", 405},
		{http.MethodPost, "/v1/lease/renew", "", 400},
		{http.MethodGet, "/v1/lease/renew", "", 405},
		{http.MethodPost, "/v1/lease/settle", "not-json", 400},
		{http.MethodDelete, "/v1/lease/settle", "", 405},
		{http.MethodPost, "/v1/observe", "", 400},
		{http.MethodGet, "/v1/observe", "", 405},
		{http.MethodGet, "/missing", "", 404},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rr := doReq(h, tt.method, tt.path, "", tt.body)
			if rr.Code != tt.want {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestUnknownJSONFieldsAllowed(t *testing.T) {
	var got quelor.AcquireRequest
	fb := &fakeBackend{
		acquireFn: func(r quelor.AcquireRequest) (quelor.AcquireResult, error) {
			got = r
			return quelor.AcquireResult{
				Verdict:     quelor.Verdict{Kind: quelor.Admit, Reason: "ok"},
				DecisionRef: "d1",
				Lease:       &quelor.Lease{ID: "L1", UnitID: r.Envelope.UnitID},
			}, nil
		},
	}
	h := server.New(fb, server.Options{})
	body := `{"envelope":{"unit_id":"u","tenant_id":"ten","holder_id":"h","extra_env":1},"token":"nope","unknown":true}`
	rr := doReq(h, http.MethodPost, "/v1/lease/acquire", "sekret", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if got.Token != "sekret" {
		t.Fatalf("token %q", got.Token)
	}
	if got.Envelope.UnitID != "u" {
		t.Fatalf("unit %q", got.Envelope.UnitID)
	}
	res := decodeBody[quelor.AcquireResult](t, rr)
	if res.Verdict.Kind != quelor.Admit || res.Lease == nil || res.Lease.ID != "L1" {
		t.Fatalf("%+v", res)
	}
}

func TestBearerTokenPropagates(t *testing.T) {
	var gotA, gotR, gotS string
	fb := &fakeBackend{
		acquireFn: func(r quelor.AcquireRequest) (quelor.AcquireResult, error) {
			gotA = r.Token
			return quelor.AcquireResult{Verdict: quelor.Verdict{Kind: quelor.Admit}}, nil
		},
		renewFn: func(r quelor.RenewRequest) (quelor.RenewResult, error) {
			gotR = r.Token
			return quelor.RenewResult{Verdict: quelor.Verdict{Kind: quelor.Admit}}, nil
		},
		settleFn: func(r quelor.SettleRequest) (quelor.SettleResult, error) {
			gotS = r.Token
			return quelor.SettleResult{LeaseID: r.LeaseID}, nil
		},
	}
	h := server.New(fb, server.Options{})
	tests := []struct {
		path string
		body string
		got  *string
	}{
		{"/v1/lease/acquire", `{"envelope":{"unit_id":"u"}}`, &gotA},
		{"/v1/lease/renew", `{"lease_id":"l","holder_id":"h"}`, &gotR},
		{"/v1/lease/settle", `{"lease_id":"l","holder_id":"h"}`, &gotS},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rr := doReq(h, http.MethodPost, tt.path, "tok", tt.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d %s", rr.Code, rr.Body.String())
			}
			if *tt.got != "tok" {
				t.Fatalf("token %q", *tt.got)
			}
		})
	}
	rr := doReq(h, http.MethodPost, "/v1/lease/acquire", "", `{"envelope":{"unit_id":"u2"}}`)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Code)
	}
	if gotA != "" {
		t.Fatalf("empty bearer stored %q", gotA)
	}
}

func TestAPIErrorMapping(t *testing.T) {
	wrappedMissing := fmt.Errorf("settle: %w", engine.ErrUnknownLease)
	tests := []struct {
		name string
		path string
		body string
		err  error
		want int
	}{
		{"acquire unauth", "/v1/lease/acquire", `{"envelope":{"unit_id":"u"}}`, engine.ErrUnauthenticated, 401},
		{"renew forbidden", "/v1/lease/renew", `{"lease_id":"l","holder_id":"h"}`, engine.ErrForbidden, 403},
		{"settle missing", "/v1/lease/settle", `{"lease_id":"l","holder_id":"h"}`, engine.ErrUnknownLease, 404},
		{"settle wrapped", "/v1/lease/settle", `{"lease_id":"l","holder_id":"h"}`, wrappedMissing, 404},
		{"observe boom", "/v1/observe", `{"resource":"slots","http_status":500}`, errors.New("boom"), 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fb := &fakeBackend{
				acquireFn: func(quelor.AcquireRequest) (quelor.AcquireResult, error) {
					return quelor.AcquireResult{}, tt.err
				},
				renewFn: func(quelor.RenewRequest) (quelor.RenewResult, error) {
					return quelor.RenewResult{}, tt.err
				},
				settleFn: func(quelor.SettleRequest) (quelor.SettleResult, error) {
					return quelor.SettleResult{}, tt.err
				},
				observeFn: func(quelor.Observation) error { return tt.err },
			}
			rr := doReq(server.New(fb, server.Options{}), http.MethodPost, tt.path, "t", tt.body)
			if rr.Code != tt.want {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["error"] == "" {
				t.Fatalf("empty error body %s", rr.Body.String())
			}
		})
	}
}

func TestVerdictHTTPStatus(t *testing.T) {
	wake := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		verdict quelor.Verdict
		want    int
	}{
		{"admit", quelor.Verdict{Kind: quelor.Admit, Reason: "ok"}, 200},
		{"defer", quelor.Verdict{Kind: quelor.Defer, Reason: "shortfall", WakeAt: &wake, WakeToken: "wt"}, 200},
		{"shed", quelor.Verdict{Kind: quelor.Shed, Reason: "preempted"}, 200},
		{"deny other", quelor.Verdict{Kind: quelor.Deny, Reason: "quota"}, 200},
		{"deny unauth holder", quelor.Verdict{Kind: quelor.Deny, Reason: "unauthenticated-holder"}, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fb := &fakeBackend{
				acquireFn: func(quelor.AcquireRequest) (quelor.AcquireResult, error) {
					return quelor.AcquireResult{Verdict: tt.verdict, DecisionRef: "d"}, nil
				},
			}
			rr := doReq(server.New(fb, server.Options{}), http.MethodPost, "/v1/lease/acquire", "", `{"envelope":{"unit_id":"u"}}`)
			if rr.Code != tt.want {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
			res := decodeBody[quelor.AcquireResult](t, rr)
			if res.Verdict.Kind != tt.verdict.Kind || res.Verdict.Reason != tt.verdict.Reason {
				t.Fatalf("verdict %+v", res.Verdict)
			}
			if tt.verdict.WakeToken != "" && res.Verdict.WakeToken != tt.verdict.WakeToken {
				t.Fatalf("wake token %q", res.Verdict.WakeToken)
			}
			if tt.verdict.WakeAt != nil {
				if res.Verdict.WakeAt == nil || !res.Verdict.WakeAt.Equal(*tt.verdict.WakeAt) {
					t.Fatalf("wake_at %+v", res.Verdict.WakeAt)
				}
			}
		})
	}
}

func TestObserveRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		body string
		want time.Duration
		code int
	}{
		{"nanos", `{"resource":"api","http_status":429,"retry_after":1500000000}`, 1500 * time.Millisecond, 429},
		{"seconds override", `{"resource":"api","http_status":503,"retry_after":99,"retry_after_seconds":2.5}`, 2500 * time.Millisecond, 503},
		{"explicit zero seconds", `{"resource":"api","http_status":200,"retry_after":99,"retry_after_seconds":0}`, 0, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got quelor.Observation
			fb := &fakeBackend{observeFn: func(o quelor.Observation) error {
				got = o
				return nil
			}}
			rr := doReq(server.New(fb, server.Options{}), http.MethodPost, "/v1/observe", "", tt.body)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
			if rr.Body.Len() != 0 {
				t.Fatalf("body %q", rr.Body.String())
			}
			if got.Resource != "api" || got.HTTPStatus != tt.code || got.RetryAfter != tt.want {
				t.Fatalf("obs %+v", got)
			}
		})
	}
}

func TestBodyLimit(t *testing.T) {
	fb := &fakeBackend{
		acquireFn: func(quelor.AcquireRequest) (quelor.AcquireResult, error) {
			return quelor.AcquireResult{Verdict: quelor.Verdict{Kind: quelor.Admit}}, nil
		},
	}
	h := server.New(fb, server.Options{})
	pad := strings.Repeat("x", 1<<20)
	body := `{"envelope":{"unit_id":"u","tenant_id":"t","holder_id":"h","affinity_key":"` + pad + `"}}`
	rr := doReq(h, http.MethodPost, "/v1/lease/acquire", "", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestHealthzAndMetricsFormat(t *testing.T) {
	fb := &fakeBackend{stats: engine.Stats{
		AcquireAdmit:     1,
		AcquireDefer:     2,
		AcquireShed:      3,
		AcquireDeny:      4,
		Renews:           5,
		RenewRefused:     6,
		Settles:          7,
		DuplicateSettles: 8,
		BoundUnreachable: 9,
		AIMDDecreases:    10,
		Expired:          11,
		TraceErrors:      12,
		ActiveLeases:     13,
	}}
	h := server.New(fb, server.Options{})

	rr := doReq(h, http.MethodGet, "/healthz", "", "")
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type %s", ct)
	}
	hz := decodeBody[struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}](t, rr)
	if hz.Status != "ok" || hz.Version != version.String() {
		t.Fatalf("%+v", hz)
	}

	// One acquire so the handler histogram moves.
	fb.acquireFn = func(quelor.AcquireRequest) (quelor.AcquireResult, error) {
		return quelor.AcquireResult{Verdict: quelor.Verdict{Kind: quelor.Admit}}, nil
	}
	if rr := doReq(h, http.MethodPost, "/v1/lease/acquire", "", `{"envelope":{"unit_id":"u"}}`); rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}

	mr := doReq(h, http.MethodGet, "/metrics", "", "")
	if mr.Code != http.StatusOK {
		t.Fatal(mr.Code)
	}
	if ct := mr.Header().Get("Content-Type"); !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("content-type %s", ct)
	}
	body := mr.Body.String()
	meta := []struct{ name, typ string }{
		{"quelor_acquire_total", "counter"},
		{"quelor_renew_total", "counter"},
		{"quelor_renew_refused_total", "counter"},
		{"quelor_settle_total", "counter"},
		{"quelor_settle_duplicate_total", "counter"},
		{"quelor_bound_unreachable_total", "counter"},
		{"quelor_aimd_decrease_total", "counter"},
		{"quelor_lease_expired_total", "counter"},
		{"quelor_trace_errors_total", "counter"},
		{"quelor_active_leases", "gauge"},
		{"quelor_acquire_duration_seconds", "histogram"},
	}
	for _, m := range meta {
		if !strings.Contains(body, "# HELP "+m.name+" ") {
			t.Errorf("missing HELP %s", m.name)
		}
		if !strings.Contains(body, "# TYPE "+m.name+" "+m.typ) {
			t.Errorf("missing TYPE %s %s", m.name, m.typ)
		}
	}
	for _, le := range []string{"0.0001", "0.0005", "0.001", "0.005", "0.01", "0.05", "0.1", "0.5", "1", "+Inf"} {
		needle := `quelor_acquire_duration_seconds_bucket{le="` + le + `"}`
		if !strings.Contains(body, needle) {
			t.Errorf("missing %s", needle)
		}
	}
	if !strings.Contains(body, "quelor_acquire_duration_seconds_count 1\n") {
		t.Fatalf("histogram count:\n%s", body)
	}
	if !strings.Contains(body, "quelor_active_leases 13\n") {
		t.Fatalf("gauge:\n%s", body)
	}
}

func TestFullFlowAcquireRenewSettle(t *testing.T) {
	h := handlerFor(t, server.Config{
		Decider:   server.DeciderConfig{Mode: "reference"},
		Resources: mustManifests(t, manifestSlots4),
	})
	ac := doReq(h, http.MethodPost, "/v1/lease/acquire", "", acquireBody(quelor.Envelope{
		UnitID:   "u1",
		TenantID: "ten",
		HolderID: "h1",
		Demands:  map[string]int64{"slots": 1},
		Priority: quelor.PriorityChange,
	}))
	if ac.Code != http.StatusOK {
		t.Fatalf("acquire %d %s", ac.Code, ac.Body.String())
	}
	ar := decodeBody[quelor.AcquireResult](t, ac)
	if ar.Verdict.Kind != quelor.Admit || ar.Lease == nil || ar.Lease.ID == "" {
		t.Fatalf("acquire result %+v", ar)
	}

	metrics := doReq(h, http.MethodGet, "/metrics", "", "").Body.String()
	if !strings.Contains(metrics, "quelor_acquire_total{verdict=\"admit\"} 1\n") {
		t.Fatalf("admit metric:\n%s", metrics)
	}
	if !strings.Contains(metrics, "quelor_active_leases 1\n") {
		t.Fatalf("active:\n%s", metrics)
	}
	if !strings.Contains(metrics, "quelor_acquire_duration_seconds_count 1\n") {
		t.Fatalf("histogram:\n%s", metrics)
	}

	rn := doReq(h, http.MethodPost, "/v1/lease/renew", "", mustJSON(quelor.RenewRequest{
		LeaseID:  ar.Lease.ID,
		HolderID: "h1",
	}))
	if rn.Code != http.StatusOK {
		t.Fatalf("renew %d %s", rn.Code, rn.Body.String())
	}
	rr := decodeBody[quelor.RenewResult](t, rn)
	if rr.Verdict.Kind != quelor.Admit || rr.Lease == nil || rr.Lease.ID != ar.Lease.ID || rr.Lease.Renewals < 1 {
		t.Fatalf("renew result %+v", rr)
	}
	metrics = doReq(h, http.MethodGet, "/metrics", "", "").Body.String()
	if !strings.Contains(metrics, "quelor_renew_total 1\n") {
		t.Fatalf("renew metric:\n%s", metrics)
	}
	if !strings.Contains(metrics, "quelor_acquire_duration_seconds_count 1\n") {
		t.Fatalf("renew recorded acquire latency:\n%s", metrics)
	}

	settle := func() quelor.SettleResult {
		t.Helper()
		rs := doReq(h, http.MethodPost, "/v1/lease/settle", "", mustJSON(quelor.SettleRequest{
			LeaseID:  ar.Lease.ID,
			HolderID: "h1",
			Consumed: map[string]int64{"slots": 1},
		}))
		if rs.Code != http.StatusOK {
			t.Fatalf("settle %d %s", rs.Code, rs.Body.String())
		}
		return decodeBody[quelor.SettleResult](t, rs)
	}
	first := settle()
	if first.Duplicate || first.LeaseID != ar.Lease.ID {
		t.Fatalf("first settle %+v", first)
	}
	if first.Declared["slots"] != 0 && first.Declared["slots"] != 1 {
		t.Fatalf("declared %+v", first.Declared)
	}
	second := settle()
	if !second.Duplicate || second.LeaseID != ar.Lease.ID {
		t.Fatalf("second settle %+v", second)
	}
	metrics = doReq(h, http.MethodGet, "/metrics", "", "").Body.String()
	if !strings.Contains(metrics, "quelor_settle_duplicate_total 1\n") {
		t.Fatalf("duplicate metric:\n%s", metrics)
	}
}

func TestDeferWhileCapacityHeld(t *testing.T) {
	h := handlerFor(t, server.Config{
		Decider:   server.DeciderConfig{Mode: "reference"},
		Resources: mustManifests(t, manifestSlots1),
	})
	hold := doReq(h, http.MethodPost, "/v1/lease/acquire", "", acquireBody(quelor.Envelope{
		UnitID:   "holder",
		TenantID: "ten",
		HolderID: "h1",
		Demands:  map[string]int64{"slots": 1},
		Priority: quelor.PriorityChange,
	}))
	if hold.Code != http.StatusOK {
		t.Fatalf("hold %d %s", hold.Code, hold.Body.String())
	}
	held := decodeBody[quelor.AcquireResult](t, hold)
	if held.Verdict.Kind != quelor.Admit {
		t.Fatalf("hold verdict %+v", held.Verdict)
	}
	dl := time.Now().Add(time.Hour)
	wait := doReq(h, http.MethodPost, "/v1/lease/acquire", "", acquireBody(quelor.Envelope{
		UnitID:   "waiter",
		TenantID: "ten",
		HolderID: "h2",
		Demands:  map[string]int64{"slots": 1},
		Priority: quelor.PriorityChange,
		Deadline: &dl,
	}))
	if wait.Code != http.StatusOK {
		t.Fatalf("wait %d %s", wait.Code, wait.Body.String())
	}
	wr := decodeBody[quelor.AcquireResult](t, wait)
	if wr.Verdict.Kind != quelor.Defer {
		t.Fatalf("want DEFER, got %+v body %s", wr.Verdict, wait.Body.String())
	}
	metrics := doReq(h, http.MethodGet, "/metrics", "", "").Body.String()
	if !strings.Contains(metrics, "quelor_acquire_total{verdict=\"defer\"} 1\n") {
		t.Fatalf("defer metric:\n%s", metrics)
	}
	if !strings.Contains(metrics, "quelor_active_leases 1\n") {
		t.Fatalf("active after defer:\n%s", metrics)
	}
}

func TestAuthUnauthenticated(t *testing.T) {
	h := handlerFor(t, server.Config{
		Decider:     server.DeciderConfig{Mode: "reference"},
		SharedToken: "secret",
	})
	body := acquireBody(quelor.Envelope{UnitID: "u", TenantID: "ten", HolderID: "h", Demands: map[string]int64{}})
	tests := []struct {
		name  string
		token string
	}{
		{"missing", ""},
		{"wrong", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := doReq(h, http.MethodPost, "/v1/lease/acquire", tt.token, body)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
		})
	}
	ok := doReq(h, http.MethodPost, "/v1/lease/acquire", "secret", body)
	if ok.Code != http.StatusOK {
		t.Fatalf("authed acquire %d %s", ok.Code, ok.Body.String())
	}
	if decodeBody[quelor.AcquireResult](t, ok).Verdict.Kind != quelor.Admit {
		t.Fatalf("verdict %s", ok.Body.String())
	}
}

func TestSettleWrongHolderForbidden(t *testing.T) {
	h := handlerFor(t, server.Config{
		Decider:      server.DeciderConfig{Mode: "reference"},
		Resources:    mustManifests(t, manifestSlots4),
		HolderTokens: map[string]string{"alice": "aaa", "bob": "bbb"},
	})
	ac := doReq(h, http.MethodPost, "/v1/lease/acquire", "aaa", acquireBody(quelor.Envelope{
		UnitID:   "u",
		TenantID: "ten",
		HolderID: "alice",
		Demands:  map[string]int64{"slots": 1},
		Priority: quelor.PriorityChange,
	}))
	if ac.Code != http.StatusOK {
		t.Fatalf("acquire %d %s", ac.Code, ac.Body.String())
	}
	ar := decodeBody[quelor.AcquireResult](t, ac)
	if ar.Verdict.Kind != quelor.Admit || ar.Lease == nil {
		t.Fatalf("acquire %+v", ar)
	}
	rs := doReq(h, http.MethodPost, "/v1/lease/settle", "bbb", mustJSON(quelor.SettleRequest{
		LeaseID:  ar.Lease.ID,
		HolderID: "bob",
	}))
	if rs.Code != http.StatusForbidden {
		t.Fatalf("settle %d %s", rs.Code, rs.Body.String())
	}
}

func TestSettleUnknownLease(t *testing.T) {
	h := handlerFor(t, server.Config{Decider: server.DeciderConfig{Mode: "reference"}})
	rr := doReq(h, http.MethodPost, "/v1/lease/settle", "", mustJSON(quelor.SettleRequest{
		LeaseID:  "does-not-exist",
		HolderID: "h1",
	}))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestObserveAIMDMetric(t *testing.T) {
	h := handlerFor(t, server.Config{
		Decider:   server.DeciderConfig{Mode: "reference"},
		Resources: mustManifests(t, manifestSlots4),
	})
	rr := doReq(h, http.MethodPost, "/v1/observe", "", `{"resource":"slots","http_status":429,"retry_after_seconds":0.25}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("observe %d %s", rr.Code, rr.Body.String())
	}
	body := doReq(h, http.MethodGet, "/metrics", "", "").Body.String()
	if !strings.Contains(body, "quelor_aimd_decrease_total 1\n") {
		t.Fatalf("aimd metric:\n%s", body)
	}
}

func TestHealthzOnEngine(t *testing.T) {
	h := handlerFor(t, server.Config{Decider: server.DeciderConfig{Mode: "reference"}})
	rr := doReq(h, http.MethodGet, "/healthz", "", "")
	got := decodeBody[map[string]string](t, rr)
	if got["status"] != "ok" || got["version"] != version.String() {
		t.Fatalf("%v", got)
	}
}

type fakeBackend struct {
	acquireFn func(quelor.AcquireRequest) (quelor.AcquireResult, error)
	renewFn   func(quelor.RenewRequest) (quelor.RenewResult, error)
	settleFn  func(quelor.SettleRequest) (quelor.SettleResult, error)
	observeFn func(quelor.Observation) error
	stats     engine.Stats
}

func (f *fakeBackend) AcquireLease(_ context.Context, r quelor.AcquireRequest) (quelor.AcquireResult, error) {
	if f.acquireFn == nil {
		return quelor.AcquireResult{}, errors.New("acquire not stubbed")
	}
	return f.acquireFn(r)
}

func (f *fakeBackend) RenewLease(_ context.Context, r quelor.RenewRequest) (quelor.RenewResult, error) {
	if f.renewFn == nil {
		return quelor.RenewResult{}, errors.New("renew not stubbed")
	}
	return f.renewFn(r)
}

func (f *fakeBackend) SettleLease(_ context.Context, r quelor.SettleRequest) (quelor.SettleResult, error) {
	if f.settleFn == nil {
		return quelor.SettleResult{}, errors.New("settle not stubbed")
	}
	return f.settleFn(r)
}

func (f *fakeBackend) Observe(_ context.Context, o quelor.Observation) error {
	if f.observeFn == nil {
		return nil
	}
	return f.observeFn(o)
}

func (f *fakeBackend) Stats() engine.Stats { return f.stats }

func handlerFor(t *testing.T, cfg server.Config) http.Handler {
	t.Helper()
	return server.New(startEngine(t, cfg), server.Options{})
}

func startEngine(t *testing.T, cfg server.Config) *engine.Engine {
	t.Helper()
	if cfg.Decider.Mode == "" {
		cfg.Decider.Mode = "reference"
	}
	if cfg.LeaseTTL == "" {
		cfg.LeaseTTL = "1h"
	}
	if cfg.TicketTTL == "" {
		cfg.TicketTTL = "1h"
	}
	if cfg.AgingStep == "" {
		cfg.AgingStep = "1s"
	}
	if cfg.DeciderTimeout == "" {
		cfg.DeciderTimeout = "2s"
	}
	if cfg.DefaultQuota.MaxLeases == 0 {
		cfg.DefaultQuota.MaxLeases = 100
	}
	eng, err := cfg.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		if err := eng.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return eng
}

func mustManifests(t *testing.T, y string) []resource.Manifest {
	t.Helper()
	var ms []resource.Manifest
	if err := yaml.Unmarshal([]byte(y), &ms); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("empty manifest")
	}
	return ms
}

func acquireBody(env quelor.Envelope) string {
	return mustJSON(quelor.AcquireRequest{Envelope: env})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func doReq(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeBody[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v body %s", err, rr.Body.String())
	}
	return v
}

// Keep bytes import used if a future table wants raw bodies; the limit test uses strings.
var _ = bytes.MinRead

// PROVES-RED: over HTTP a superseded run must differ from a success. Before
// SettleResult carried rule_id, both bodies omitted the field.
func TestSettleJSONRuleIDSuperseded(t *testing.T) {
	h := handlerFor(t, server.Config{
		Decider:   server.DeciderConfig{Mode: "reference"},
		Resources: mustManifests(t, manifestSlots4),
	})
	acquire := func(unit string, gen int64) quelor.Lease {
		t.Helper()
		rr := doReq(h, http.MethodPost, "/v1/lease/acquire", "", acquireBody(quelor.Envelope{
			UnitID: unit, TenantID: "ten", HolderID: "h1",
			Demands:     map[string]int64{"slots": 1},
			Priority:    quelor.PriorityChange,
			CoalesceKey: "pr-1", Generation: gen,
		}))
		ar := decodeBody[quelor.AcquireResult](t, rr)
		if rr.Code != http.StatusOK || ar.Lease == nil {
			t.Fatalf("acquire %d %s", rr.Code, rr.Body.String())
		}
		return *ar.Lease
	}
	settleRaw := func(l quelor.Lease, out *quelor.ExecutionOutcome) map[string]any {
		t.Helper()
		rr := doReq(h, http.MethodPost, "/v1/lease/settle", "", mustJSON(quelor.SettleRequest{
			LeaseID: l.ID, HolderID: "h1", Outcome: out,
		}))
		if rr.Code != http.StatusOK {
			t.Fatalf("settle %d %s", rr.Code, rr.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	old := acquire("run-1", 1)
	live := acquire("run-2", 2)

	sup := settleRaw(old, nil)
	if sup["rule_id"] != "superseded" {
		t.Fatalf("superseded body %v", sup)
	}
	if _, has := sup["class"]; has {
		t.Fatalf("superseded body carries class: %v", sup)
	}
	ok := settleRaw(live, &quelor.ExecutionOutcome{})
	if ok["rule_id"] != "success" {
		t.Fatalf("success body %v", ok)
	}
}
