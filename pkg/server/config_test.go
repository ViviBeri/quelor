package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/server"
	"net/http"
)

func TestLoadConfigStrict(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"unknown top level", "listen: 127.0.0.1:1\nnope: true\n"},
		{"unknown decider field", "decider:\n  mode: reference\n  nope: 1\n"},
		{"unknown quota field", "default_quota:\n  max_leases: 1\n  nope: 1\n"},
		{"unknown shadow field", "shadows:\n  - url: http://127.0.0.1:9\n    nope: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := server.LoadConfig(p); err == nil {
				t.Fatal("expected strict-parse error")
			}
		})
	}
}

func TestLoadConfigDefaultsAndQuota(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	doc := `
decider:
  mode: reference
default_quota:
  max_leases: 3
  max_units:
    slots: 9
holder_quotas:
  alice:
    max_leases: 1
    max_units:
      slots: 2
cell: cell-a
lease_ttl: 30s
ticket_ttl: 5s
aging_step: 1s
decider_timeout: 2s
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := server.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Fatalf("listen %q", cfg.Listen)
	}
	if cfg.Cell != "cell-a" || cfg.LeaseTTL != "30s" || cfg.TicketTTL != "5s" || cfg.AgingStep != "1s" || cfg.DeciderTimeout != "2s" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.DefaultQuota.MaxLeases != 3 || cfg.DefaultQuota.MaxUnits["slots"] != 9 {
		t.Fatalf("default quota %+v", cfg.DefaultQuota)
	}
	if cfg.HolderQuotas["alice"].MaxLeases != 1 || cfg.HolderQuotas["alice"].MaxUnits["slots"] != 2 {
		t.Fatalf("holder quota %+v", cfg.HolderQuotas["alice"])
	}

	explicit := filepath.Join(dir, "explicit.yaml")
	if err := os.WriteFile(explicit, []byte("listen: 127.0.0.1:9090\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = server.LoadConfig(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9090" {
		t.Fatalf("listen %q", cfg.Listen)
	}
}

func TestBuildRuleset(t *testing.T) {
	dir := t.TempDir()
	rules := filepath.Join(dir, "ruleset.yaml")
	if err := os.WriteFile(rules, []byte("id: test\nrules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "c.yaml")
	doc := "decider:\n  mode: reference\n  ruleset: " + rules + "\nlease_ttl: 1h\n"
	if err := os.WriteFile(cfgPath, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := server.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decider.Ruleset != rules {
		t.Fatalf("ruleset path %q", cfg.Decider.Ruleset)
	}
	eng, err := cfg.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if eng == nil {
		t.Fatal("nil engine")
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  server.Config
	}{
		{"bad mode", server.Config{Decider: server.DeciderConfig{Mode: "nope"}}},
		{"bad lease ttl", server.Config{Decider: server.DeciderConfig{Mode: "reference"}, LeaseTTL: "yesterday"}},
		{"negative ttl", server.Config{Decider: server.DeciderConfig{Mode: "reference"}, TicketTTL: "-1s"}},
		{"http missing url", server.Config{Decider: server.DeciderConfig{Mode: "http"}}},
		{"bad decider timeout", server.Config{Decider: server.DeciderConfig{Mode: "http", URL: "http://127.0.0.1:9", Timeout: "nope"}}},
		{"shadow missing url", server.Config{Decider: server.DeciderConfig{Mode: "reference"}, Shadows: []server.ShadowConfig{{}}}},
		{"missing ruleset", server.Config{Decider: server.DeciderConfig{Mode: "reference", Ruleset: filepath.Join(t.TempDir(), "missing.yaml")}}},
		{"missing resource file", server.Config{Decider: server.DeciderConfig{Mode: "reference"}, ResourceFiles: []string{filepath.Join(t.TempDir(), "missing.yaml")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.cfg.Build(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCountResourcesMissingFile(t *testing.T) {
	cfg := server.Config{ResourceFiles: []string{filepath.Join(t.TempDir(), "nope.yaml")}}
	if _, err := cfg.CountResources(); err == nil {
		t.Fatal("expected error")
	}
}

func TestSharedTokenEnv(t *testing.T) {
	const envName = "QUELOR_TEST_SHARED_TOKEN"
	t.Setenv(envName, "env-token")
	h := handlerFor(t, server.Config{
		Decider:        server.DeciderConfig{Mode: "reference"},
		SharedToken:    "file-token",
		SharedTokenEnv: envName,
	})
	body := acquireBody(quelor.Envelope{
		UnitID:   "u",
		TenantID: "ten",
		HolderID: "h",
		Demands:  map[string]int64{},
	})
	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"env token", "env-token", http.StatusOK},
		{"file token ignored", "file-token", http.StatusUnauthorized},
		{"missing", "", http.StatusUnauthorized},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rr := doReq(h, http.MethodPost, "/v1/lease/acquire", tt.token, body)
			if rr.Code != tt.want {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSharedTokenEnvFallback(t *testing.T) {
	h := handlerFor(t, server.Config{
		Decider:        server.DeciderConfig{Mode: "reference"},
		SharedToken:    "file-token",
		SharedTokenEnv: "QUELOR_TEST_SHARED_TOKEN_UNSET",
	})
	body := acquireBody(quelor.Envelope{
		UnitID:   "u",
		TenantID: "ten",
		HolderID: "h",
		Demands:  map[string]int64{},
	})
	ok := doReq(h, http.MethodPost, "/v1/lease/acquire", "file-token", body)
	if ok.Code != http.StatusOK {
		t.Fatalf("fallback %d %s", ok.Code, ok.Body.String())
	}
	no := doReq(h, http.MethodPost, "/v1/lease/acquire", "", body)
	if no.Code != http.StatusUnauthorized {
		t.Fatalf("missing %d %s", no.Code, no.Body.String())
	}
}

func TestLoadConfigDoesNotEchoToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("shared_token: super-secret\nnot_a_field: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := server.LoadConfig(p)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("token leaked in error: %v", err)
	}
}
