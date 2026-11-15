// Package configs loads the server configuration from a JSON file.
package configs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that unmarshals from JSON strings such as "5s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Size is a byte count that unmarshals from a JSON number or a string such as
// "64MiB" (KiB/MiB/GiB are powers of 1024; KB/MB/GB are powers of 1000).
type Size int64

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
	{"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3}, {"b", 1},
}

func (s *Size) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*s = Size(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return errors.New("size must be a number of bytes or a string like \"64MiB\"")
	}
	str = strings.ToLower(strings.TrimSpace(str))
	mult := int64(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(str, u.suffix) {
			str, mult = strings.TrimSpace(strings.TrimSuffix(str, u.suffix)), u.mult
			break
		}
	}
	v, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid size: %w", err)
	}
	*s = Size(v * mult)
	return nil
}

// Cache configures the in-memory file cache. A zero MaxSize disables it.
type Cache struct {
	// MaxSize bounds the total size of cached file contents.
	MaxSize Size `json:"max_size"`
	// MaxEntrySize is the largest file cached (default: MaxSize).
	MaxEntrySize Size `json:"max_entry_size"`
	// TTL is how long a file is served from memory before being reloaded
	// from disk; zero keeps it until evicted.
	TTL Duration `json:"ttl"`
	// Eviction picks what to drop when full: "fifo", "lifo" or "largest".
	Eviction string `json:"eviction"`
}

type Timeouts struct {
	ReadHeader Duration `json:"read_header"`
	Read       Duration `json:"read"`
	Write      Duration `json:"write"`
	Idle       Duration `json:"idle"`
	Shutdown   Duration `json:"shutdown"`
	// Drain is how long readiness reports 503 after SIGTERM before the
	// server stops accepting connections, giving Kubernetes time to remove
	// the pod from Service endpoints.
	Drain Duration `json:"drain"`
}

// Metrics configures the Prometheus endpoint, served on its own listener so
// it is not exposed with the public content. An empty Address disables it.
type Metrics struct {
	Address string `json:"address"`
	Path    string `json:"path"`
}

// Probes configures the Kubernetes probe endpoints. An empty path disables it.
type Probes struct {
	LivenessPath  string `json:"liveness_path"`
	ReadinessPath string `json:"readiness_path"`
}

type Config struct {
	// Address to listen on, e.g. ":8080".
	Address string `json:"address"`
	// Root directory of the static content.
	Root    string  `json:"root"`
	Probes  Probes  `json:"probes"`
	Metrics Metrics `json:"metrics"`
	// AccessLog enables one JSON log line per request.
	AccessLog bool `json:"access_log"`
	// Headers are added to every static file response.
	Headers map[string]string `json:"headers"`
	// HeaderRules add or override headers for matching files, in order.
	HeaderRules []HeaderRule `json:"header_rules"`
	// Fallback is served with 200 for missing extensionless paths (SPAs).
	Fallback string `json:"fallback"`
	// NotFoundPage is served with 404 for missing files.
	NotFoundPage string `json:"not_found_page"`
	// Precompressed serves "<file>.br"/"<file>.gz" siblings when accepted.
	Precompressed bool     `json:"precompressed"`
	Cache         Cache    `json:"cache"`
	Timeouts      Timeouts `json:"timeouts"`
}

// HeaderRule matches files with path.Match syntax. A pattern without "/"
// matches the base name ("*.html"); otherwise the full path ("/assets/*").
type HeaderRule struct {
	Match   string            `json:"match"`
	Headers map[string]string `json:"headers"`
}

func Default() Config {
	return Config{
		Address: ":8080",
		Root:    "/srv/www",
		Probes: Probes{
			LivenessPath:  "/livez",
			ReadinessPath: "/readyz",
		},
		Metrics: Metrics{
			Address: ":9090",
			Path:    "/metrics",
		},
		Cache: Cache{Eviction: "fifo", TTL: Duration(5 * time.Minute)},
		Timeouts: Timeouts{
			ReadHeader: Duration(5 * time.Second),
			Read:       Duration(10 * time.Second),
			Write:      Duration(60 * time.Second),
			Idle:       Duration(120 * time.Second),
			Shutdown:   Duration(15 * time.Second),
			Drain:      Duration(5 * time.Second),
		},
	}
}

// Load returns the defaults overlaid with the JSON file at path.
// An empty path returns the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	var errs []error
	if c.Address == "" {
		errs = append(errs, errors.New("address is required"))
	}
	if c.Root == "" {
		errs = append(errs, errors.New("root is required"))
	}
	for name, p := range map[string]string{
		"probes.liveness_path": c.Probes.LivenessPath, "probes.readiness_path": c.Probes.ReadinessPath,
	} {
		if p != "" && (!strings.HasPrefix(p, "/") || strings.ContainsAny(p, " {}")) {
			errs = append(errs, fmt.Errorf("%s must be a plain path starting with /", name))
		}
	}
	if c.Probes.LivenessPath != "" && c.Probes.LivenessPath == c.Probes.ReadinessPath {
		errs = append(errs, errors.New("probes.liveness_path and probes.readiness_path must differ"))
	}
	if c.Metrics.Address != "" {
		if c.Metrics.Address == c.Address {
			errs = append(errs, errors.New("metrics.address must differ from address"))
		}
		if !strings.HasPrefix(c.Metrics.Path, "/") || strings.ContainsAny(c.Metrics.Path, " {}") {
			errs = append(errs, errors.New("metrics.path must be a plain path starting with /"))
		}
	}
	for i, r := range c.HeaderRules {
		if r.Match == "" {
			errs = append(errs, fmt.Errorf("header_rules[%d].match is required", i))
		} else if _, err := path.Match(r.Match, ""); err != nil {
			errs = append(errs, fmt.Errorf("header_rules[%d].match %q: %w", i, r.Match, err))
		}
	}
	for name, p := range map[string]string{"fallback": c.Fallback, "not_found_page": c.NotFoundPage} {
		if p != "" && (!strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/" ||
			strings.Contains(p, "/.")) {
			errs = append(errs, fmt.Errorf("%s must be a clean file path starting with /, e.g. /index.html", name))
		}
	}
	switch c.Cache.Eviction {
	case "fifo", "lifo", "largest":
	default:
		errs = append(errs, fmt.Errorf("cache.eviction must be fifo, lifo or largest, got %q", c.Cache.Eviction))
	}
	if c.Cache.MaxSize < 0 || c.Cache.MaxEntrySize < 0 || c.Cache.TTL < 0 {
		errs = append(errs, errors.New("cache.max_size, cache.max_entry_size and cache.ttl must not be negative"))
	}
	t := c.Timeouts
	for name, d := range map[string]Duration{
		"read_header": t.ReadHeader, "read": t.Read, "write": t.Write,
		"idle": t.Idle, "shutdown": t.Shutdown, "drain": t.Drain,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("timeouts.%s must not be negative", name))
		}
	}
	return errors.Join(errs...)
}
