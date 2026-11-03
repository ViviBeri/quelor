// Package server is the HTTP front end for the Quelor admission engine.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/coherera/quelor/pkg/engine"
	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/version"
)

// maxBody is the accepted request body size.
const maxBody = 1 << 20

// maxRetryAfter is the longest retry delay an observation may report.
const maxRetryAfter = 24 * time.Hour

// Backend is the engine surface the daemon calls.
type Backend interface {
	quelor.LeaseAPI
	Observe(ctx context.Context, o quelor.Observation) error
	// Authenticate reports whether token is valid for holder. A backend that
	// cannot tell must return false: observe is denied by default.
	Authenticate(holder, token string) bool
	Stats() engine.Stats
}

// Options configures the HTTP handler. The zero value is ready to use.
type Options struct{}

type srv struct {
	be   Backend
	hist *histogram
}

// New returns the lease HTTP handler. Verdicts, including DEFER, SHED, and
// DENY, are written as 200 JSON. A DENY whose reason is unauthenticated-holder
// is 401, with the same JSON body.
func New(be Backend, _ Options) http.Handler {
	s := &srv{be: be, hist: newHistogram(acquireBuckets)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/lease/acquire", s.acquire)
	mux.HandleFunc("POST /v1/lease/renew", s.renew)
	mux.HandleFunc("POST /v1/lease/settle", s.settle)
	mux.HandleFunc("POST /v1/observe", s.observe)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

func (s *srv) acquire(w http.ResponseWriter, r *http.Request) {
	var req quelor.AcquireRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Token = bearerToken(r)
	start := time.Now()
	res, err := s.be.AcquireLease(r.Context(), req)
	s.hist.observe(time.Since(start).Seconds())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeDecision(w, res.Verdict, res)
}

func (s *srv) renew(w http.ResponseWriter, r *http.Request) {
	var req quelor.RenewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Token = bearerToken(r)
	res, err := s.be.RenewLease(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeDecision(w, res.Verdict, res)
}

func (s *srv) settle(w http.ResponseWriter, r *http.Request) {
	var req quelor.SettleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Token = bearerToken(r)
	res, err := s.be.SettleLease(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// observeBody accepts Observation plus an optional seconds override.
// RetryAfter on the wire is nanoseconds; retry_after_seconds wins when present.
// The caller authenticates like any other holder: holder_id in the body and
// the bearer token in the header. Retry delays above 24 hours, negative values
// and non-finite values are rejected with 400.
type observeBody struct {
	HolderID          string   `json:"holder_id,omitempty"`
	Resource          string   `json:"resource"`
	HTTPStatus        int      `json:"http_status"`
	RetryAfter        int64    `json:"retry_after,omitempty"`
	RetryAfterSeconds *float64 `json:"retry_after_seconds,omitempty"`
}

func (s *srv) observe(w http.ResponseWriter, r *http.Request) {
	var body observeBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if !s.be.Authenticate(body.HolderID, bearerToken(r)) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": engine.ErrUnauthenticated.Error()})
		return
	}
	retry := time.Duration(0)
	switch {
	case body.RetryAfterSeconds != nil:
		secs := *body.RetryAfterSeconds
		if math.IsNaN(secs) || secs < 0 || secs > maxRetryAfter.Seconds() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "retry_after_seconds out of range"})
			return
		}
		retry = time.Duration(secs * float64(time.Second))
	default:
		if body.RetryAfter < 0 || body.RetryAfter > int64(maxRetryAfter) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "retry_after out of range"})
			return
		}
		retry = time.Duration(body.RetryAfter)
	}
	obs := quelor.Observation{
		Resource:   body.Resource,
		HTTPStatus: body.HTTPStatus,
		RetryAfter: retry,
	}
	if err := s.be.Observe(r.Context(), obs); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *srv) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.String(),
	})
}

func (s *srv) metrics(w http.ResponseWriter, r *http.Request) {
	text := renderMetrics(s.be.Stats(), s.hist.snapshot())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, text)
}

func bearerToken(r *http.Request) string {
	const scheme = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) < len(scheme) || !strings.EqualFold(v[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(v[len(scheme):])
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return false
	}
	return true
}

func writeDecision(w http.ResponseWriter, v quelor.Verdict, body any) {
	code := http.StatusOK
	if v.Kind == quelor.Deny && v.Reason == "unauthenticated-holder" {
		code = http.StatusUnauthorized
	}
	writeJSON(w, code, body)
}

func writeAPIError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, engine.ErrUnauthenticated):
		code = http.StatusUnauthorized
	case errors.Is(err, engine.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, engine.ErrUnknownLease):
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
