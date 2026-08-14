// Package master implements the master-side HTTP surface for cluster mode:
// slave registration, config distribution, and inbound cycle ingestion. It
// plugs into the existing api router as a sub-handler mounted under
// /api/v1/cluster.
package master

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// Caps on ingest body size. Register is tiny JSON; /cycles carries at most
// ~600 cycles × a few hundred bytes, so 100 MiB is a paranoid upper bound
// that still stops a compromised bearer from exhausting memory.
const (
	maxRegisterBody = 64 << 10  // 64 KiB
	maxCyclesBody   = 100 << 20 // 100 MiB
)

// Server wires the three cluster endpoints against the master's config store,
// slave registry, and downstream Sink (usually the same Fanout local cycles
// feed into, so slave data reaches Writer + Evaluator + LogSink identically).
type Server struct {
	log      *slog.Logger
	store    *config.Store
	registry *Registry
	sink     scheduler.Sink
	health   func() *slavehealth.Set
}

// currentToken returns the bearer secret the master accepts right now. It is
// read from the store per request rather than captured at construction, so a
// SIGHUP that rotates cluster.token takes effect immediately. An absent
// cluster block returns "", which BearerAuth treats as deny-all.
func (s *Server) currentToken() string {
	cfg := s.store.Current()
	if cfg.Cluster == nil {
		return ""
	}
	return cfg.Cluster.Token
}

// NewServer builds a master-side cluster handler. The bearer secret is read
// from store per request (see currentToken), not passed in, so rotating
// cluster.token over SIGHUP takes effect without a restart; a store whose
// config carries no token denies every cluster request.
//
// health is a live accessor rather than a snapshot: the mesh changes as
// slaves register, but the Server is constructed once at startup. Pass nil
// for standalone tests and deployments with no health mesh wired.
func NewServer(log *slog.Logger, store *config.Store, registry *Registry, sink scheduler.Sink, health func() *slavehealth.Set) *Server {
	return &Server{
		log:      log,
		store:    store,
		registry: registry,
		sink:     sink,
		health:   health,
	}
}

// healthSet returns the current mesh snapshot, or nil when health probing is
// not wired (standalone tests, or a master with no registry).
func (s *Server) healthSet() *slavehealth.Set {
	if s.health == nil {
		return nil
	}
	return s.health()
}

// Handler returns the sub-router with bearer auth already applied. Mount it
// at /api/v1/cluster from the main API router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(cluster.BearerAuth(s.currentToken))
	r.Post("/register", s.handleRegister)
	r.Get("/config", s.handleConfig)
	r.Post("/cycles", s.handleCycles)
	return r
}

const maxSlaveNameLen = 128

// validSlaveName gates the identity a slave can claim. A valid bearer token
// authenticates the request but does not bind it to a name, so the name is
// untrusted and must be validated wherever it is consumed (register, config,
// cycles) — not just at /register. "master" is reserved both to keep the
// registry honest and because it is the source label of local probes: a slave
// stamping source="master" would collide with and corrupt the master's own
// data and alert series. Control characters are rejected so the label stays
// clean in ClickHouse and the alert exec environment.
func validSlaveName(name string) bool {
	if name == "" || len(name) > maxSlaveNameLen {
		return false
	}
	if name == "master" {
		return false
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRegisterBody)
	var req cluster.RegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !validSlaveName(req.Name) {
		http.Error(w, `name required: ≤128 bytes, not "master", no control chars`, http.StatusBadRequest)
		return
	}
	s.registry.Touch(req.Name, req.Version, r.RemoteAddr, req.Advertise)
	s.log.Info("slave registered", "name", req.Name, "version", req.Version, "addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, cluster.RegisterResp{Ack: true})
}

// handleConfig serves the scrubbed cluster config for the named slave. The
// slave is identified by X-Slave-Name header — the bearer token authenticates
// the request, the header scopes the filter. Missing header returns the
// unfiltered view (debug-friendly for `curl`).
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	slaveName := r.Header.Get("X-Slave-Name")
	if slaveName != "" {
		if !validSlaveName(slaveName) {
			http.Error(w, "invalid slave name", http.StatusBadRequest)
			return
		}
		s.registry.Touch(slaveName, r.Header.Get("X-Slave-Version"), r.RemoteAddr, r.Header.Get(cluster.HeaderAdvertise))
	}

	resp := BuildClusterConfig(cfg, slaveName, s.healthSet())
	etag := cluster.ETag(resp)
	if etag != "" && r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCycles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCyclesBody)
	var batch cluster.CycleBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// Prefer the authenticated X-Slave-Name header over the wire-provided
	// batch.Source — a valid token does not bind a slave identity, so any
	// slave could otherwise forge another's source label and corrupt alert
	// state or registry entries. Fall back to batch.Source for older slaves
	// that don't send the header.
	name, version := r.Header.Get("X-Slave-Name"), r.Header.Get("X-Slave-Version")
	if name == "" {
		// Legacy slaves omit the header; fall back to the wire-provided source.
		name, version = batch.Source, ""
	}
	if name != "" {
		if !validSlaveName(name) {
			http.Error(w, "invalid slave name", http.StatusBadRequest)
			return
		}
		s.registry.Touch(name, version, r.RemoteAddr, r.Header.Get(cluster.HeaderAdvertise))
		batch.Source = name
	}
	n := s.ingestBatch(r, batch)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": n})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
