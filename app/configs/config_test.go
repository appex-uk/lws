package configs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != ":8080" || cfg.Root != "/srv/www" || cfg.Probes.LivenessPath != "/livez" ||
		cfg.Probes.ReadinessPath != "/readyz" ||
		cfg.Metrics != (Metrics{Address: ":9090", Path: "/metrics"}) {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		wantErr bool
		check   func(t *testing.T, c Config)
	}{
		{
			name: "overrides and keeps defaults",
			body: `{"address":":9000","headers":{"X-A":"1"},"timeouts":{"read":"3s"}}`,
			check: func(t *testing.T, c Config) {
				if c.Address != ":9000" || c.Root != "/srv/www" || c.Headers["X-A"] != "1" {
					t.Errorf("got %+v", c)
				}
				if time.Duration(c.Timeouts.Read) != 3*time.Second ||
					time.Duration(c.Timeouts.Write) != 60*time.Second {
					t.Errorf("timeouts %+v", c.Timeouts)
				}
			},
		},
		{name: "unknown field", body: `{"port":80}`, wantErr: true},
		{name: "bad duration", body: `{"timeouts":{"read":"soon"}}`, wantErr: true},
		{name: "numeric duration", body: `{"timeouts":{"read":5}}`, wantErr: true},
		{name: "negative duration", body: `{"timeouts":{"idle":"-1s"}}`, wantErr: true},
		{name: "empty root", body: `{"root":""}`, wantErr: true},
		{name: "relative probe path", body: `{"probes":{"liveness_path":"livez"}}`, wantErr: true},
		{name: "probe path with pattern", body: `{"probes":{"readiness_path":"/{x}"}}`, wantErr: true},
		{name: "same probe paths", body: `{"probes":{"liveness_path":"/h","readiness_path":"/h"}}`, wantErr: true},
		{name: "metrics disabled", body: `{"metrics":{"address":""}}`, check: func(t *testing.T, c Config) {
			if c.Metrics.Address != "" {
				t.Errorf("metrics %+v", c.Metrics)
			}
		}},
		{name: "metrics same address", body: `{"address":":8080","metrics":{"address":":8080"}}`, wantErr: true},
		{name: "metrics bad path", body: `{"metrics":{"path":"metrics"}}`, wantErr: true},
		{name: "negative drain", body: `{"timeouts":{"drain":"-1s"}}`, wantErr: true},
		{name: "probes disabled", body: `{"probes":{"liveness_path":"","readiness_path":""}}`, check: func(t *testing.T, c Config) {
			if c.Probes != (Probes{}) {
				t.Errorf("probes %+v", c.Probes)
			}
		}},
		{name: "invalid json", body: `{`, wantErr: true},
		{name: "header rule ok", body: `{"header_rules":[{"match":"*.html","headers":{"Cache-Control":"no-cache"}}]}`,
			check: func(t *testing.T, c Config) {
				if len(c.HeaderRules) != 1 || c.HeaderRules[0].Headers["Cache-Control"] != "no-cache" {
					t.Errorf("rules %+v", c.HeaderRules)
				}
			}},
		{name: "header rule empty match", body: `{"header_rules":[{"match":""}]}`, wantErr: true},
		{name: "header rule bad pattern", body: `{"header_rules":[{"match":"["}]}`, wantErr: true},
		{name: "fallback ok", body: `{"fallback":"/index.html","not_found_page":"/errors/404.html"}`},
		{name: "fallback relative", body: `{"fallback":"index.html"}`, wantErr: true},
		{name: "fallback root", body: `{"fallback":"/"}`, wantErr: true},
		{name: "fallback unclean", body: `{"fallback":"/a/../index.html"}`, wantErr: true},
		{name: "not found dotfile", body: `{"not_found_page":"/.hidden/404.html"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(writeFile(t, tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected error")
	}
}

func TestShippedConfigsAreValid(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"config.json", "config.local.json"} {
		if _, err := Load(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
