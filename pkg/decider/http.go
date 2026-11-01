package decider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	deciderAPIHeader     = "X-Quelor-Decider-API"
	defaultClientTimeout = 250 * time.Millisecond
	maxBodyBytes         = 1 << 20 // 1 MiB
)

// ErrUnavailable is the sentinel for a Decider that could not answer.
var ErrUnavailable = errors.New("decider unavailable")

var errBodyTooLarge = errors.New("body exceeds 1MiB")

// HTTPClient calls a remote Decider over HTTP.
//
// Timeout defaults to 250ms when zero or negative and bounds the call with
// the request context. A nil HTTP client uses http.DefaultTransport and that
// same Timeout. Non-2xx responses, transport errors, timeouts, undecodable
// bodies, oversized bodies, and a mismatched decider_api_version yield
// StatusUnavailable and an error wrapping ErrUnavailable.
type HTTPClient struct {
	URL       string
	HTTP      *http.Client
	Timeout   time.Duration
	DeciderID string
}

// Decide POSTs req as JSON and decodes the Response JSON.
func (c *HTTPClient) Decide(ctx context.Context, req Request) (Response, error) {
	if c == nil {
		return Unavailable("", "nil http client"), fmt.Errorf("%w: nil http client", ErrUnavailable)
	}
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.effectiveTimeout())
	defer cancel()

	payload, err := json.Marshal(req)
	if err != nil {
		return c.fail(time.Since(start), fmt.Errorf("marshal request: %w", err))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return c.fail(time.Since(start), fmt.Errorf("build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(deciderAPIHeader, APIVersion)

	resp, err := c.httpClient().Do(httpReq)
	elapsed := time.Since(start)
	if err != nil {
		return c.fail(elapsed, err)
	}
	defer resp.Body.Close()

	raw, err := readCapped(resp.Body)
	if err != nil {
		return c.fail(elapsed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := raw
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return c.fail(elapsed, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return c.fail(elapsed, fmt.Errorf("decode response: %w", err))
	}
	if out.DeciderAPIVersion != "" && out.DeciderAPIVersion != APIVersion {
		return c.fail(elapsed, fmt.Errorf("decider_api_version %q", out.DeciderAPIVersion))
	}
	if out.LatencyMS == 0 {
		out.LatencyMS = latencyMS(elapsed)
	}
	return out, nil
}

func (c *HTTPClient) fail(elapsed time.Duration, cause error) (Response, error) {
	resp := Unavailable(c.DeciderID, cause.Error())
	resp.LatencyMS = latencyMS(elapsed)
	return resp, fmt.Errorf("%w: %w", ErrUnavailable, cause)
}

func (c *HTTPClient) effectiveTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultClientTimeout
}

func (c *HTTPClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: c.effectiveTimeout()}
}

func latencyMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func readCapped(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, errors.New("nil body")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	return raw, nil
}

// Handler is the sidecar HTTP server for d. POST exchanges the Decider JSON
// contract. Any other method is 405. Malformed or oversized bodies are 400.
// An error from d is 503 with {"error":"..."}; a normal Response, including
// abstain or unavailable status, is 200.
func Handler(d Decider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body := r.Body
		if body == nil {
			body = io.NopCloser(bytes.NewReader(nil))
		}
		raw, err := readCapped(body)
		if err != nil {
			msg := "invalid request body"
			if errors.Is(err, errBodyTooLarge) {
				msg = err.Error()
			}
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		var decReq Request
		if err := json.Unmarshal(raw, &decReq); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if d == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "nil decider"})
			return
		}
		resp, err := d.Decide(r.Context(), decReq)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	out, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
