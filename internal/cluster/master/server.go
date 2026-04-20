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
	token    string
}

// NewServer builds a master-side cluster handler. token is the shared bearer
// secret checked on every request; an empty token disables the mount at the
// caller level (we still require non-empty here as a safety).
func NewServer(log *slog.Logger, store *config.Store, registry *Registry, sink scheduler.Sink, token string) *Server {
	return &Server{
		log:      log,
		store:    store,
		registry: registry,
		sink:     sink,
		token:    token,
	}
}

// Handler returns the sub-router with bearer auth already applied. Mount it
// at /api/v1/cluster from the main API router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(cluster.BearerAuth(s.token))
	r.Post("/register", s.handleRegister)
	r.Get("/config", s.handleConfig)
	r.Post("/cycles", s.handleCycles)
	return r
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRegisterBody)
	var req cluster.RegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	s.registry.Touch(req.Name, req.Version, r.RemoteAddr)
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
		s.registry.Touch(slaveName, r.Header.Get("X-Slave-Version"), r.RemoteAddr)
	}

	resp := BuildClusterConfig(cfg, slaveName)
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
	if batch.Source != "" {
		s.registry.Touch(batch.Source, "", r.RemoteAddr)
	}
	n := s.ingestBatch(r, batch)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": n})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
