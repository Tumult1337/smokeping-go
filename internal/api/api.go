package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// SlaveLister reports the names of slaves currently registered with the
// master. Master mode plugs the cluster Registry in here; standalone and
// slave mode leave it nil.
type SlaveLister interface {
	Names() []string
}

type Server struct {
	log            *slog.Logger
	store          *config.Store
	reader         storage.Reader
	uiFS           fs.FS
	clusterHandler http.Handler
	slaves         SlaveLister
	startAt        time.Time
}

type Options struct {
	Log    *slog.Logger
	Store  *config.Store
	Reader storage.Reader
	// UIFS is the filesystem holding the built SPA (index.html + assets/).
	// May be nil — routes will 404 for UI paths in that case.
	UIFS fs.FS
	// ClusterHandler is the master-side sub-router for /api/v1/cluster/*. Nil
	// in standalone or slave mode; set when the master exposes cluster endpoints.
	ClusterHandler http.Handler
	// Slaves is the live slave registry used to compute /sources. Nil when
	// not in master mode.
	Slaves SlaveLister
}

func New(opts Options) *Server {
	return &Server{
		log:            opts.Log,
		store:          opts.Store,
		reader:         opts.Reader,
		uiFS:           opts.UIFS,
		clusterHandler: opts.ClusterHandler,
		slaves:         opts.Slaves,
		startAt:        time.Now(),
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(logRequests(s.log))

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.health)
		r.Get("/sources", s.listSources)
		r.Get("/targets", s.listTargets)
		// Target IDs are group/name so routes must match two segments.
		r.Get("/targets/{group}/{name}/cycles", s.getCycles)
		r.Get("/targets/{group}/{name}/rtts", s.getRTTs)
		r.Get("/targets/{group}/{name}/http", s.getHTTP)
		r.Get("/targets/{group}/{name}/status", s.getStatus)
		r.Get("/targets/{group}/{name}/hops", s.getHops)
		r.Get("/targets/{group}/{name}/hops/timeline", s.getHopsTimeline)
		if s.clusterHandler != nil {
			r.Mount("/cluster", s.clusterHandler)
		}
	})

	if s.uiFS != nil {
		fileServer := http.FileServer(http.FS(s.uiFS))
		r.Get("/", s.serveIndex)
		r.Get("/assets/*", fileServer.ServeHTTP)
		r.Get("/favicon.ico", fileServer.ServeHTTP)
		r.NotFound(s.serveIndex) // SPA fallback
	}
	return r
}

// Serve runs the HTTP server and blocks until ctx is cancelled, then gives the
// server up to 5s to finish in-flight requests.
func Serve(ctx context.Context, log *slog.Logger, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout is generous because /api/v1/cluster/cycles accepts up to
		// 100 MiB from slaves over potentially slow links. WriteTimeout covers
		// the widest Flux query we expect (1d bucket, max window).
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", addr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"uptime":  time.Since(s.startAt).String(),
		"version": "dev",
	})
}

type targetDTO struct {
	ID         string   `json:"id"`
	Group      string   `json:"group"`
	GroupTitle string   `json:"group_title,omitempty"`
	Name       string   `json:"name"`
	Title      string   `json:"title,omitempty"`
	Probe      string   `json:"probe"`
	ProbeType  string   `json:"probe_type,omitempty"`
	Host       string   `json:"host,omitempty"`
	URL        string   `json:"url,omitempty"`
	Alerts     []string `json:"alerts,omitempty"`
	// Sources lists the probe origins that actually ping this target right
	// now: the master (when it probes locally) plus every registered slave
	// that is either unassigned globally or named in the target's Slaves
	// list. The UI uses this to render per-target source chips instead of a
	// single global list.
	Sources []string `json:"sources,omitempty"`
}

// listSources returns the set of distinct probe origins the UI can filter on:
// the master's own source stamp plus every slave currently registered. Since
// slaves probe every target, the same list applies to every row in the UI.
func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	list := []string{masterSourceName(cfg)}
	if s.slaves != nil {
		list = append(list, s.slaves.Names()...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": list})
}

// masterSourceName returns the string the master stamps on its own locally
// probed cycles — `cluster.source` if set, else "master".
func masterSourceName(cfg *config.Config) string {
	if cfg.Cluster != nil && cfg.Cluster.Source != "" {
		return cfg.Cluster.Source
	}
	return "master"
}

func (s *Server) listTargets(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	// Build a group→title map so we can echo g.Title on every target without
	// scanning the group list for each one.
	groupTitles := make(map[string]string, len(cfg.Targets))
	for _, g := range cfg.Targets {
		groupTitles[g.Group] = g.Title
	}

	masterSource := masterSourceName(cfg)
	var registered []string
	if s.slaves != nil {
		registered = s.slaves.Names()
	}

	out := make([]targetDTO, 0)
	for _, t := range cfg.AllTargets() {
		pt := ""
		if p, ok := cfg.Probes[t.Target.Probe]; ok {
			pt = p.Type
		}
		out = append(out, targetDTO{
			ID:         t.ID(),
			Group:      t.Group,
			GroupTitle: groupTitles[t.Group],
			Name:       t.Target.Name,
			Title:      t.Target.Title,
			Probe:      t.Target.Probe,
			ProbeType:  pt,
			Host:       t.Target.Host,
			URL:        t.Target.URL,
			Alerts:     t.Target.Alerts,
			Sources:    effectiveSources(t.Target, masterSource, registered),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// effectiveSources returns the probe origins that currently ping this target.
// Unassigned targets (empty t.Slaves) are probed by the master plus every
// registered slave. Assigned targets are probed only by the named slaves;
// the master skips them locally so it's excluded. Assigned slaves that
// haven't registered are omitted — they're not actually probing yet.
func effectiveSources(t config.Target, masterSource string, registered []string) []string {
	if len(t.Slaves) == 0 {
		out := make([]string, 0, len(registered)+1)
		out = append(out, masterSource)
		out = append(out, registered...)
		return out
	}
	assigned := make(map[string]struct{}, len(t.Slaves))
	for _, s := range t.Slaves {
		assigned[s] = struct{}{}
	}
	out := make([]string, 0, len(t.Slaves))
	for _, s := range registered {
		if _, ok := assigned[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) getCycles(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	from, to, ok := parseRange(w, r, 24*time.Hour)
	if !ok {
		return
	}
	res := pickResolution(r.URL.Query().Get("resolution"), from, to)
	filter := storage.QueryFilter{Source: r.URL.Query().Get("source")}

	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	points, err := s.reader.QueryCycles(r.Context(), ref, from, to, res, filter)
	if err != nil {
		s.log.Warn("query cycles", "err", err)
		writeErr(w, http.StatusBadGateway, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resolution": res,
		"from":       from,
		"to":         to,
		"points":     points,
	})
}

func (s *Server) getRTTs(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	from, to, ok := parseRange(w, r, time.Hour)
	if !ok {
		return
	}
	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	points, err := s.reader.QueryRTTs(r.Context(), ref, from, to, storage.QueryFilter{Source: r.URL.Query().Get("source")})
	if err != nil {
		s.log.Warn("query rtts", "err", err)
		writeErr(w, http.StatusBadGateway, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from":   from,
		"to":     to,
		"points": points,
	})
}

func (s *Server) getHTTP(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	from, to, ok := parseRange(w, r, 24*time.Hour)
	if !ok {
		return
	}
	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	// HTTP samples live only in the raw bucket. 7d matches raw retention and
	// keeps one "1y"-click from scanning a giant series.
	if to.Sub(from) > 7*24*time.Hour {
		writeErr(w, http.StatusBadRequest, "http window limited to 7d")
		return
	}
	points, err := s.reader.QueryHTTPSamples(r.Context(), ref, from, to, storage.QueryFilter{Source: r.URL.Query().Get("source")})
	if err != nil {
		s.log.Warn("query http", "err", err)
		writeErr(w, http.StatusBadGateway, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target": ref.ID(),
		"from":   from,
		"to":     to,
		"points": points,
	})
}

func (s *Server) getHops(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}

	// `at` is an optional unix-seconds/RFC3339 timestamp. When present we pick
	// the single cycle closest to it (within ±30m) so the UI can show the
	// hops view from any moment of the main chart. Absent = latest.
	var hops []storage.HopPoint
	var err error
	filter := storage.QueryFilter{Source: r.URL.Query().Get("source")}
	if atStr := r.URL.Query().Get("at"); atStr != "" {
		at, perr := parseTimeParam(atStr, time.Time{}, time.Now())
		if perr != nil {
			writeErr(w, http.StatusBadRequest, "invalid at: expected RFC3339, unix seconds, or duration like -1h")
			return
		}
		hops, err = s.reader.QueryHopsAt(r.Context(), ref, at, 30*time.Minute, filter)
	} else {
		hops, err = s.reader.QueryLatestHops(r.Context(), ref, filter)
	}
	if err != nil {
		s.log.Warn("query hops", "err", err)
		writeErr(w, http.StatusBadGateway, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": ref.ID(), "hops": hops})
}

func (s *Server) getHopsTimeline(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	from, to, ok := parseRange(w, r, 24*time.Hour)
	if !ok {
		return
	}
	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	// Hop data only lives in the raw bucket (no rollups). Reject windows wider
	// than raw retention so a "1y" click doesn't try to scan 100M points.
	if to.Sub(from) > 7*24*time.Hour {
		writeErr(w, http.StatusBadRequest, "hops/timeline window limited to 7d")
		return
	}
	hops, err := s.reader.QueryHopsTimeline(r.Context(), ref, from, to, storage.QueryFilter{Source: r.URL.Query().Get("source")})
	if err != nil {
		s.log.Warn("query hops timeline", "err", err)
		writeErr(w, http.StatusBadGateway, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target": ref.ID(),
		"from":   from,
		"to":     to,
		"hops":   hops,
	})
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	// Show the last 50 cycles from the raw bucket.
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	points, err := s.reader.QueryCycles(r.Context(), ref, from, to, storage.ResolutionRaw, storage.QueryFilter{Source: r.URL.Query().Get("source")})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "query failed")
		return
	}
	if len(points) > 50 {
		points = points[len(points)-50:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": ref.ID(), "recent": points})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if s.uiFS == nil {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.uiFS, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) resolveTarget(w http.ResponseWriter, r *http.Request) (config.TargetRef, bool) {
	group := chi.URLParam(r, "group")
	name := chi.URLParam(r, "name")
	id := group + "/" + name
	cfg := s.store.Current()
	for _, t := range cfg.AllTargets() {
		if t.ID() == id {
			return t, true
		}
	}
	writeErr(w, http.StatusNotFound, fmt.Sprintf("target %q not found", id))
	return config.TargetRef{}, false
}

func parseRange(w http.ResponseWriter, r *http.Request, defaultSpan time.Duration) (time.Time, time.Time, bool) {
	q := r.URL.Query()
	now := time.Now()
	from, err := parseTimeParam(q.Get("from"), now.Add(-defaultSpan), now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid from: expected RFC3339, unix seconds, or duration like -1h")
		return time.Time{}, time.Time{}, false
	}
	to, err := parseTimeParam(q.Get("to"), now, now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid to: expected RFC3339, unix seconds, or duration like -1h")
		return time.Time{}, time.Time{}, false
	}
	if !to.After(from) {
		writeErr(w, http.StatusBadRequest, "to must be after from")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// parseTimeParam accepts RFC3339, a unix timestamp, or a relative duration
// like "-1h" (interpreted from `now`). Empty returns the default.
func parseTimeParam(s string, def, now time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		d, err := parseRelativeDuration(s)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	ts, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("not rfc3339, duration, or unix: %q", s)
	}
	return time.Unix(ts, 0), nil
}

// parseRelativeDuration extends time.ParseDuration with "d" (days) and "w"
// (weeks) so UI-friendly windows like "-7d" and "-365d" work. Go's stdlib
// only parses up to "h", which would reject anything wider than a day.
func parseRelativeDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// Replace trailing "d"/"w" with their hour equivalent, then retry.
	// Only the last unit is replaced — compound forms ("1d6h") aren't used.
	switch {
	case strings.HasSuffix(s, "d"):
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "d"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	case strings.HasSuffix(s, "w"):
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "w"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("invalid duration %q", s)
}

func pickResolution(override string, from, to time.Time) storage.Resolution {
	switch override {
	case "raw":
		return storage.ResolutionRaw
	case "1h":
		return storage.Resolution1h
	case "1d":
		return storage.Resolution1d
	}
	return storage.PickResolution(from, to)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func logRequests(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			log.Debug("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"dur", time.Since(start))
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
