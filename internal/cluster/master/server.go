// Package master implements the master-side HTTP surface for cluster mode:
// slave registration, config distribution, and inbound cycle ingestion. It
// plugs into the existing api router as a sub-handler mounted under
// /api/v1/cluster.
package master

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

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
	dedup    *cycleDedup
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
	dedup := newCycleDedup()
	// The registry is the list of names ingest will accept, so it is also the
	// lifecycle a dedup window follows: swept names lose theirs, and eviction
	// spends a dead window before a live one's.
	dedup.registered = registry.Has
	registry.SetOnRemove(dedup.forgetSource)
	return &Server{
		log:      log,
		store:    store,
		registry: registry,
		sink:     sink,
		health:   health,
		dedup:    dedup,
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

const maxSlaveNameLen = config.MaxSlaveNameLen

// validSlaveName gates the identity a slave can claim. A valid bearer token
// authenticates the request but does not bind it to a name, so the name is
// untrusted and must be validated wherever it is consumed (register, config,
// cycles) — not just at /register. "master" is reserved both to keep the
// registry honest and because it is the source label of local probes: a slave
// stamping source="master" would collide with and corrupt the master's own
// data and alert series. Control characters are rejected so the label stays
// clean in ClickHouse and the alert exec environment.
// validSlaveName is config.ValidSlaveName; the rule lives there so
// Config.Validate can refuse a name this would, rather than a subset of it.
func validSlaveName(name string) bool { return config.ValidSlaveName(name) }

// refusePermanently answers 400 and marks it as the master's own verdict, so
// a slave can tell it from the same status arriving out of an intermediary.
func refusePermanently(w http.ResponseWriter, msg string) {
	w.Header().Set(cluster.HeaderRefusal, cluster.RefusalPermanent)
	http.Error(w, msg, http.StatusBadRequest)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRegisterBody)
	var req cluster.RegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		refusePermanently(w, "invalid json")
		return
	}
	if !validSlaveName(req.Name) {
		refusePermanently(w, `name required: ≤128 bytes, not "master", no control chars`)
		return
	}
	if err := s.registry.Touch(req.Name, req.Version, r.RemoteAddr, req.Advertise); err != nil {
		// Registry-full is the retryable capacity condition; anything else is
		// this request's own bytes and resending them can never succeed.
		if errors.Is(err, errRegistryFull) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		refusePermanently(w, err.Error())
		return
	}
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
			refusePermanently(w, "invalid slave name")
			return
		}
		_ = s.registry.Touch(slaveName, r.Header.Get("X-Slave-Version"), r.RemoteAddr, r.Header.Get(cluster.HeaderAdvertise))
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
		refusePermanently(w, "invalid json")
		return
	}
	// The claimed identity comes from X-Slave-Name, falling back to
	// batch.Source for slaves that predate the header — both are wire values
	// the shared token does not bind, so the registry, not the request, is
	// what decides the name exists.
	name, version := r.Header.Get("X-Slave-Name"), r.Header.Get("X-Slave-Version")
	if name == "" {
		name, version = batch.Source, ""
	}
	if !validSlaveName(name) {
		refusePermanently(w, `name required: ≤128 bytes, not "master", no control chars`)
		return
	}
	// Checked before Touch, which would otherwise create the very entry it is
	// being asked about. An unregistered name is refused rather than minted:
	// the source label reaches ClickHouse as a LowCardinality dictionary
	// entry, becomes a row QueryLatestHops carries forever, and surfaces on
	// the unauthenticated API as an origin that never existed.
	if !s.registry.Has(name) {
		http.Error(w, "unregistered slave: POST /register first", http.StatusForbidden)
		return
	}
	if err := batch.Validate(time.Now()); err != nil {
		s.log.Warn("rejecting cluster batch outside ingest bounds", "slave", name, "err", err)
		refusePermanently(w, "batch outside ingest bounds")
		return
	}
	_ = s.registry.Touch(name, version, r.RemoteAddr, r.Header.Get(cluster.HeaderAdvertise))
	batch.Source = name
	n, dup := s.ingestBatch(r, batch)
	if dup > 0 {
		// At most one line per push, so the rate is the slave's flush cadence:
		// sustained duplicates mean acks are being lost between the two peers.
		s.log.Info("cluster ingest skipped redelivered cycles",
			"slave", name, "duplicates", dup, "accepted", n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": n, "duplicate": dup})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
