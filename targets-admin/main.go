// Command targets-admin serves a small web UI and JSON API for managing the
// list of iperf servers that Prometheus should continuously probe. It
// persists targets in the Prometheus file_sd_config JSON format so
// Prometheus picks up changes automatically, without a restart.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const testDialTimeout = 3 * time.Second

const (
	defaultPort = 5201

	// defaultPeriod and maxPeriod bound the iperf3 test duration (seconds) a
	// target can request. The exporter now serializes download/upload probes
	// against the same target:port (they queue instead of racing — iperf3's
	// server only handles one client at a time), so the worst case for one
	// direction is waiting up to maxPeriod for the other to finish, then
	// running its own maxPeriod — i.e. 2×maxPeriod. maxPeriod=20 must stay
	// comfortably below the scrape_timeout configured for the
	// iperf3-download/iperf3-upload jobs in prometheus/prometheus.yml (50s
	// = 2×20 + buffer) — otherwise Prometheus aborts the scrape before the
	// (possibly queued) iperf3 test finishes.
	defaultPeriod = 5
	maxPeriod     = 20

	// defaultInterval and minInterval bound how often (seconds) a target is
	// probed. minInterval must stay above the same scrape_timeout (50s) —
	// Prometheus rejects a scrape_interval shorter than scrape_timeout for
	// that target — so a margin is kept on top of it.
	defaultInterval = 60
	minInterval     = 60
	maxInterval     = 3600

	// defaultStreams and maxStreams bound the number of parallel iperf3
	// streams (-P) a target can request. maxStreams mirrors the exporter's
	// own MaxStreams cap (internal/iperf/iperf.go in the iperf3_exporter
	// fork) — kept in sync by hand since it's a separate repo.
	defaultStreams = 1
	maxStreams     = 64
)

//go:embed static/*
var staticFS embed.FS

var (
	errConflict = errors.New("conflict")
	errNotFound = errors.New("not found")

	hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
)

// Target is the admin-facing representation of an iperf server.
type Target struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Period   int    `json:"period"`
	Interval int    `json:"interval"`
	Streams  int    `json:"streams"`
}

// sdGroup mirrors the Prometheus file_sd_config target group format, which
// is the on-disk representation Prometheus itself reads.
type sdGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// Store guards read-modify-write access to the targets file and keeps it in
// sync with what Prometheus expects on disk.
type Store struct {
	mu   sync.Mutex
	path string
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create targets directory: %w", err)
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := s.write(nil); err != nil {
			return nil, fmt.Errorf("seed targets file: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat targets file: %w", err)
	}

	// Rewrite the file once so any target written by an older version of
	// this program (missing labels such as "period") gets backfilled with
	// current defaults. Safe to run unconditionally: read() already
	// resolves defaults, so this is a no-op when the file is up to date.
	targets, err := s.read()
	if err != nil {
		return nil, fmt.Errorf("read targets file: %w", err)
	}

	if err := s.write(targets); err != nil {
		return nil, fmt.Errorf("normalize targets file: %w", err)
	}

	return s, nil
}

func (s *Store) List() ([]Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.read()
}

func (s *Store) Add(t Target) (Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	targets, err := s.read()
	if err != nil {
		return Target{}, err
	}

	for _, existing := range targets {
		if strings.EqualFold(existing.Name, t.Name) {
			return Target{}, fmt.Errorf("%w: name %q already exists", errConflict, t.Name)
		}
	}

	targets = append(targets, t)
	if err := s.write(targets); err != nil {
		return Target{}, err
	}

	return t, nil
}

func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	targets, err := s.read()
	if err != nil {
		return err
	}

	kept := targets[:0]
	found := false

	for _, existing := range targets {
		if strings.EqualFold(existing.Name, name) {
			found = true
			continue
		}

		kept = append(kept, existing)
	}

	if !found {
		return errNotFound
	}

	return s.write(kept)
}

func (s *Store) read() ([]Target, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read targets file: %w", err)
	}

	var groups []sdGroup
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, fmt.Errorf("parse targets file: %w", err)
	}

	targets := make([]Target, 0, len(groups))

	for _, g := range groups {
		if len(g.Targets) == 0 {
			continue
		}

		port, _ := strconv.Atoi(g.Labels["port"])
		if port == 0 {
			port = defaultPort
		}

		period, _ := strconv.Atoi(g.Labels["period"])
		if period == 0 {
			period = defaultPeriod
		}

		interval, _ := strconv.Atoi(g.Labels["interval"])
		if interval == 0 {
			interval = defaultInterval
		}

		streams, _ := strconv.Atoi(g.Labels["streams"])
		if streams == 0 {
			streams = defaultStreams
		}

		targets = append(targets, Target{
			Name:     g.Labels["name"],
			Host:     g.Targets[0],
			Port:     port,
			Period:   period,
			Interval: interval,
			Streams:  streams,
		})
	}

	return targets, nil
}

// write persists targets atomically (temp file + rename) so Prometheus,
// which polls this file on a timer, never observes a partially written file.
func (s *Store) write(targets []Target) error {
	groups := make([]sdGroup, 0, len(targets))

	for _, t := range targets {
		groups = append(groups, sdGroup{
			Targets: []string{t.Host},
			Labels: map[string]string{
				"name":     t.Name,
				"port":     strconv.Itoa(t.Port),
				"period":   strconv.Itoa(t.Period),
				"interval": strconv.Itoa(t.Interval),
				"streams":  strconv.Itoa(t.Streams),
			},
		})
	}

	data, err := json.MarshalIndent(groups, "", "  ")
	if err != nil {
		return fmt.Errorf("encode targets file: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temp targets file: %w", err)
	}

	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace targets file: %w", err)
	}

	return nil
}

// normalize trims and validates user input, applying the default iperf3
// port when none was supplied.
func normalize(t Target) (Target, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.Host = strings.TrimSpace(t.Host)

	if t.Name == "" {
		return Target{}, errors.New("name is required")
	}

	if len(t.Name) > 63 {
		return Target{}, errors.New("name must be 63 characters or fewer")
	}

	host, port, err := validateHostPort(t.Host, t.Port)
	if err != nil {
		return Target{}, err
	}

	t.Host, t.Port = host, port

	if t.Period == 0 {
		t.Period = defaultPeriod
	}

	if t.Period < 1 || t.Period > maxPeriod {
		return Target{}, fmt.Errorf("period must be between 1 and %d seconds", maxPeriod)
	}

	if t.Interval == 0 {
		t.Interval = defaultInterval
	}

	if t.Interval < minInterval || t.Interval > maxInterval {
		return Target{}, fmt.Errorf("scrape interval must be between %d and %d seconds", minInterval, maxInterval)
	}

	if t.Streams == 0 {
		t.Streams = defaultStreams
	}

	if t.Streams < 1 || t.Streams > maxStreams {
		return Target{}, fmt.Errorf("streams must be between 1 and %d", maxStreams)
	}

	return t, nil
}

// validateHostPort applies the default iperf3 port when none is given and
// checks that host/port are well-formed. Shared by target creation and the
// connectivity test, so both reject the same malformed input.
func validateHostPort(host string, port int) (string, int, error) {
	host = strings.TrimSpace(host)

	if host == "" {
		return "", 0, errors.New("host is required")
	}

	if net.ParseIP(host) == nil && !hostnamePattern.MatchString(host) {
		return "", 0, errors.New("host must be a valid IP address or hostname")
	}

	if port == 0 {
		port = defaultPort
	}

	if port < 1 || port > 65535 {
		return "", 0, errors.New("port must be between 1 and 65535")
	}

	return host, port, nil
}

func main() {
	targetsFile := getEnv("TARGETS_FILE", "/data/iperf_servers.json")
	listenAddr := getEnv("LISTEN_ADDR", ":8080")

	store, err := NewStore(targetsFile)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}

	staticFiles, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("embed static assets: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/targets", store.handleList)
	mux.HandleFunc("POST /api/targets", store.handleCreate)
	mux.HandleFunc("DELETE /api/targets/{name}", store.handleDelete)
	mux.HandleFunc("POST /api/test-target", handleTestTarget)
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.Handle("GET /", http.FileServerFS(staticFiles))

	log.Printf("targets-admin listening on %s (targets file: %s)", listenAddr, targetsFile)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Store) handleList(w http.ResponseWriter, _ *http.Request) {
	targets, err := s.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, targets)
}

func (s *Store) handleCreate(w http.ResponseWriter, r *http.Request) {
	var t Target
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	t, err := normalize(t)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	created, err := s.Add(t)
	if err != nil {
		if errors.Is(err, errConflict) {
			writeError(w, http.StatusConflict, err)
			return
		}

		writeError(w, http.StatusInternalServerError, err)

		return
	}

	writeJSON(w, http.StatusCreated, created)
}

func (s *Store) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if err := s.Delete(name); err != nil {
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}

		writeError(w, http.StatusInternalServerError, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleTestTarget checks TCP reachability of host:port before the user
// commits to adding it as a target. A plain ICMP ping is not a useful proxy
// here: many WAN hosts block ICMP but still serve iperf3 on TCP, and what
// actually matters is whether iperf3_exporter will be able to open the
// iperf3 port — so this dials that port directly instead of pinging.
func handleTestTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	host, port, err := validateHostPort(req.Host, req.Port)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	start := time.Now()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), testDialTimeout)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer conn.Close()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latencyMs": time.Since(start).Milliseconds()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
