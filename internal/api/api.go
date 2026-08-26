package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
	"github.com/tumult/gosmokeping/internal/storage"
)

// SlaveLister reports the names of slaves currently registered with the
// master. Master mode plugs the cluster Registry in here; standalone and
// slave mode leave it nil.
type SlaveLister interface {
	Names() []string
}

// HealthLister reports the synthetic slave-health targets, with addresses
// already stripped. The API is deliberately wired to this narrow interface
// rather than to the registry: there is no accessor here that could return an
// address, so no handler can leak one.
type HealthLister interface {
	PublicTargets() []config.TargetRef
}

// WriterStats exposes the storage writer's per-table drop counters to
// /health; nil when no writer is wired (slave mode, storage disabled).
type WriterStats interface {
	Dropped() map[string]uint64
}

type Server struct {
	log            *slog.Logger
	store          *config.Store
	reader         storage.Reader
	uiFS           fs.FS
	clusterHandler http.Handler
	slaves         SlaveLister
	healthLister   HealthLister
	writerStats    WriterStats
	version        string
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
	// Health lists synthetic slave-health targets. Nil in standalone and
	// slave mode; set when the master runs a health mesh.
	Health HealthLister
	// WriterStats reports the storage writer's drop counters on /health. Nil
	// in slave mode and when storage is disabled.
	WriterStats WriterStats
	// Version is the build version reported by /health. Empty falls back to "dev".
	Version string
}

func New(opts Options) *Server {
	v := opts.Version
	if v == "" {
		v = "dev"
	}
	return &Server{
		log:            opts.Log,
		store:          opts.Store,
		reader:         opts.Reader,
		uiFS:           opts.UIFS,
		clusterHandler: opts.ClusterHandler,
		slaves:         opts.Slaves,
		healthLister:   opts.Health,
		writerStats:    opts.WriterStats,
		version:        v,
		startAt:        time.Now(),
	}
}

// healthTargets returns the address-stripped health targets, or nil when
// health probing is not wired.
func (s *Server) healthTargets() []config.TargetRef {
	if s.healthLister == nil {
		return nil
	}
	return s.healthLister.PublicTargets()
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(logRequests(s.log))
	// gzip JSON responses. A 24h all-sources hops/timeline is ~28 MB raw
	// JSON; gzip cuts that to ~3 MB. A reverse proxy (Cloudflare, nginx)
	// may already compress, but we can't assume one — direct origin
	// access regresses badly without this. Level 5 is the sweet spot for
	// JSON: ≥80% of the size win at ~half the CPU of level 9. Negotiates
	// only when the client sent Accept-Encoding, so non-gzip clients are
	// unaffected.
	//
	// Cluster ingest (/cluster/cycles) accepts large POST bodies but
	// returns trivially small responses — gzip on the response path is
	// effectively a no-op for it.
	r.Use(middleware.Compress(5, "application/json"))

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
		r.Get("/overview", s.getOverview)
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
		// 100 MiB from slaves over potentially slow links. WriteTimeout has to
		// accommodate the slowest read path: a 7d hops/timeline query against
		// the raw tier can legitimately take tens of seconds against
		// ClickHouse on large datasets. Anything tighter cancels the response
		// before ClickHouse can finish, surfaces as 502 to the UI, and
		// prevents CachingReader from ever warming the entry.
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
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
	payload := map[string]any{
		"status":  "ok",
		"uptime":  time.Since(s.startAt).String(),
		"version": s.version,
	}
	if s.writerStats != nil {
		payload["writer_drops"] = s.writerStats.Dropped()
	}
	writeJSON(w, http.StatusOK, payload)
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
	// Health targets live outside the stored config, so they are appended
	// here. Sources is every registered slave plus the master: the mesh is
	// full, minus each node probing itself, and the difference does not
	// change the filter chips the UI renders.
	for _, t := range s.healthTargets() {
		out = append(out, targetDTO{
			ID:         t.ID(),
			Group:      t.Group,
			GroupTitle: slavehealth.GroupTitle,
			Name:       t.Target.Name,
			Title:      t.Target.Title,
			Probe:      t.Target.Probe,
			ProbeType:  "icmp",
			// Alerts are operator-chosen labels with no address content, and
			// omitting them would render a health target as unmonitored while
			// it is in fact alerting.
			Alerts: t.Target.Alerts,
			// Host and URL stay zero: the whole point of the Public view.
			Sources: healthSources(t.Target.Name, masterSource, registered),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// healthSources lists the nodes that probe a given health target: the master
// plus every registered slave except the target itself, which never probes
// its own address.
func healthSources(targetName, masterSource string, registered []string) []string {
	out := make([]string, 0, len(registered)+1)
	out = append(out, masterSource)
	for _, s := range registered {
		if s == targetName {
			continue
		}
		out = append(out, s)
	}
	return out
}

// sourcesFor returns the probe origins for any target ref, health or not.
// Health targets need healthSources rather than effectiveSources: their
// Slaves list is empty, so effectiveSources would claim a slave probes its
// own health target — which it never does.
func sourcesFor(t config.TargetRef, masterSource string, registered []string) []string {
	if slavehealth.IsHealthGroup(t.Group) {
		return healthSources(t.Target.Name, masterSource, registered)
	}
	return effectiveSources(t.Target, masterSource, registered)
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
	step, ok := pickStep(r.URL.Query().Get("step"), from, to)
	if !ok {
		writeErr(w, http.StatusBadRequest, "requested step is finer than the bucket ladder serves for this window; narrow the range or drop the step override")
		return
	}

	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	points, err := s.reader.QueryCycles(r.Context(), ref, from, to, storage.QueryFilter{Source: r.URL.Query().Get("source"), Step: step})
	if err != nil {
		s.writeQueryErr(w, "query cycles", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from":   from,
		"to":     to,
		"points": points,
	})
}

// maxRTTWindow bounds /rtts. It is deliberately tighter than the 7d the
// sibling raw endpoints allow: those store one row per cycle or per request,
// where probe_rtt stores one per ping.
const maxRTTWindow = 24 * time.Hour

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
	// probe_rtt holds one row per ping, making this the densest response the
	// API produces; without a cap an anonymous request scans the whole
	// retention window. Nothing in the UI calls this endpoint — raw RTT only
	// informs the smoke band at short zooms.
	if to.Sub(from) > maxRTTWindow {
		writeErr(w, http.StatusBadRequest, "rtts window limited to 24h")
		return
	}
	points, err := s.reader.QueryRTTs(r.Context(), ref, from, to, storage.QueryFilter{Source: r.URL.Query().Get("source")})
	if err != nil {
		s.writeQueryErr(w, "query rtts", err)
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
		s.writeQueryErr(w, "query http", err)
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

	// `at` pins the cycle nearest it within ±15m — half the window passed
	// below — and only its RFC3339 form carries the sub-second precision that
	// names one cycle at a sub-2s cadence; absent = latest.
	var res storage.HopsResult
	var err error
	filter := storage.QueryFilter{Source: r.URL.Query().Get("source")}
	if atStr := r.URL.Query().Get("at"); atStr != "" {
		at, perr := parseTimeParam(atStr, time.Time{}, time.Now())
		if perr != nil {
			writeErr(w, http.StatusBadRequest, "invalid at: "+perr.Error())
			return
		}
		res, err = s.reader.QueryHopsAt(r.Context(), ref, at, 30*time.Minute, filter)
	} else {
		// Drop sources that have gone silent from the current-path view, using
		// the same staleness threshold the overview uses for its silent flag.
		// A removed slave's last path otherwise renders as live until its hop
		// rows age out of retention (~90d).
		if interval := s.store.Current().Interval; interval > 0 {
			filter.LatestSince = time.Now().Add(-time.Duration(silentCycleMultiplier) * interval)
		}
		res, err = s.reader.QueryLatestHops(r.Context(), ref, filter)
	}
	if err != nil {
		s.writeQueryErr(w, "query hops", err)
		return
	}
	hops := res.Hops
	if slavehealth.IsHealthGroup(ref.Group) {
		hops = redactTerminalHops(hops)
	}
	// target_loss is a sibling of hops, not a field on one: a hop row's
	// sent/lost counts probes at a single TTL, and reading that as the
	// target's loss is the bug this key exists to retire. A source whose
	// cycle sent nothing has no entry rather than a zeroed one.
	writeJSON(w, http.StatusOK, map[string]any{
		"target":      ref.ID(),
		"hops":        hops,
		"target_loss": cycleCounterDTOs(res.Cycles),
	})
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
	// The window the grid's row bound is derived from, so it is that bound's
	// constant rather than a second copy of 7d.
	if to.Sub(from) > storage.MaxHopTimelineWindow {
		writeErr(w, http.StatusBadRequest, "hops/timeline window limited to 7d")
		return
	}
	// One probe origin per request. The window and the step ladder bound the
	// grid, and the ttl column bounds its other axis, but the number of sources
	// with rows in the window is bounded by nothing — so it is admitted rather
	// than derived. An empty value is a source too: the untagged pre-cluster
	// origin. The heatmap already draws and fetches one canvas per source.
	if !r.URL.Query().Has("source") {
		writeErr(w, http.StatusBadRequest, "source is required: hops/timeline serves one probe origin per request")
		return
	}
	// The probe interval floors the step: a grid finer than the cadence that
	// fills it leaves empty columns the heatmap draws as a stopped probe.
	step := storage.PickHopStep(to.Sub(from), s.store.Current().Interval)
	res, err := s.reader.QueryHopsTimeline(r.Context(), ref, from, to, storage.QueryFilter{
		Source: r.URL.Query().Get("source"),
		Step:   step,
	})
	if err != nil {
		s.writeQueryErr(w, "query hops timeline", err)
		return
	}
	hops := res.Hops
	if slavehealth.IsHealthGroup(ref.Group) {
		hops = redactAllHopAddresses(hops)
	}
	// Slim DTO: the heatmap renders only LossPct + MaxLossPct, so the
	// per-row RTT fields (Min/Max/Mean/Median) the storage row carries
	// for the path-table view get dropped here. Saves ~40% of the JSON
	// payload over the wire — multiplicative with bucketing for wide
	// windows. Path table at /hops?at= keeps the full HopPoint shape.
	dtos := make([]hopTimelineDTO, len(hops))
	for i, h := range hops {
		dtos[i] = hopTimelineDTO{
			Time:       h.Time,
			Source:     h.Source,
			Index:      h.Index,
			IP:         h.IP,
			Unreach:    h.Unreach,
			LossPct:    h.LossPct,
			MaxLossPct: h.MaxLossPct,
			LossCount:  h.LossCount,
			Sent:       h.Sent,
			WorstTime:  h.WorstTime,
		}
	}
	// step_sec echoes the bucket width the ladder picked, always positive.
	// The heatmap draws one column per bucket and cannot infer that width from
	// the rows: a window holding a single bucket carries no gap to measure, and
	// guessing from row count paints that bucket across the whole window.
	writeJSON(w, http.StatusOK, map[string]any{
		"target":   ref.ID(),
		"from":     from,
		"to":       to,
		"step_sec": int64(step / time.Second),
		"hops":     dtos,
	})
}

// cycleCounterDTOs serves storage.CycleCounters as-is (its five fields are
// exactly the target_loss wire shape) and always returns a non-nil slice so
// the key is an empty array rather than JSON null when no cycle recorded a
// measurement.
func cycleCounterDTOs(cycles []storage.CycleCounters) []storage.CycleCounters {
	if cycles == nil {
		return []storage.CycleCounters{}
	}
	return cycles
}

// hopTimelineDTO is the wire shape returned by /hops/timeline. Distinct
// from storage.HopPoint because the heatmap consumer only needs loss
// fields — shipping RTT min/max/mean/median per row balloons a 24h
// all-sources response by ~40% for no rendering gain. Field names stay
// PascalCase to match HopPoint so the TS client treats them identically.
type hopTimelineDTO struct {
	Time       time.Time `json:"Time"`
	Source     string    `json:"Source"`
	Index      int64     `json:"Index"`
	IP         string    `json:"IP"`
	LossPct    float64   `json:"LossPct"`
	MaxLossPct float64   `json:"MaxLossPct"`
	LossCount  int64     `json:"LossCount"`
	Sent       int64     `json:"Sent"`
	// Unreach is the closed-set unreachable label aggregated over the bucket.
	// TargetReply deliberately has no counterpart here: the heatmap has no
	// consumer for it, and a field with no consumer on an unauthenticated
	// endpoint is pure disclosure surface.
	Unreach string `json:"Unreach"`
	// WorstTime: exact timestamp of the bucket's worst-loss cycle so a
	// heatmap click can open that cycle instead of the bucket's first one.
	WorstTime time.Time `json:"WorstTime"`
}

// redactTerminalHops blanks, per (source, timestamp) group, the union of:
//   - every TargetReply row (a per-round walk can put the target's echo below
//     a deeper all-silent round, where no positional rule finds it),
//   - the furthest-index row (covers rows written before the marker existed),
//   - every row sharing an address with either (a TimeExceeded quoting the
//     target's own address on an unmarked row).
//
// A group in which no arm yields an address has no identifiable terminal, so
// every address in it is blanked — that group-wide arm is what makes the
// set-membership arms fail closed against a token holder crafting a silent
// terminal. Every comparison is against the served rows themselves, never a
// configured address, so the Probe/Public split holds, and addresses compare
// as parsed values (canonHopAddr) so spelling variants cannot split a row
// from its mate. A blanked row loses Unreach with its address but keeps
// TargetReply, which discloses nothing the surviving rows do not already
// carry. Intermediates survive on purpose — /hops feeds the MTR path table;
// /hops/timeline blanks everything via redactAllHopAddresses instead.
//
// Groups key on UnixMilli, the reader's own timestamp identity (DateTime64(3)
// stores ms), and stay per (source, timestamp) even though both current
// callers pin one timestamp per source: over multi-trace rows a source-wide
// maximum would leave every shorter trace's terminal unredacted.
func redactTerminalHops(hops []storage.HopPoint) []storage.HopPoint {
	if len(hops) == 0 {
		return hops
	}
	type sourceTime struct {
		source string
		unix   int64
	}
	maxIndex := make(map[sourceTime]int64, 4)
	for _, h := range hops {
		key := sourceTime{h.Source, h.Time.UnixMilli()}
		if cur, ok := maxIndex[key]; !ok || h.Index > cur {
			maxIndex[key] = h.Index
		}
	}
	targetIPs := make(map[sourceTime]map[hopAddr]bool, len(maxIndex))
	for _, h := range hops {
		key := sourceTime{h.Source, h.Time.UnixMilli()}
		if (h.TargetReply || h.Index == maxIndex[key]) && h.IP != "" {
			if targetIPs[key] == nil {
				targetIPs[key] = make(map[hopAddr]bool, 1)
			}
			targetIPs[key][canonHopAddr(h.IP)] = true
		}
	}
	out := make([]storage.HopPoint, len(hops))
	copy(out, hops)
	for i := range out {
		key := sourceTime{out[i].Source, out[i].Time.UnixMilli()}
		mates := targetIPs[key]
		if out[i].TargetReply || out[i].Index == maxIndex[key] || len(mates) == 0 ||
			(out[i].IP != "" && mates[canonHopAddr(out[i].IP)]) {
			out[i].IP = ""
			out[i].Unreach = ""
		}
	}
	return out
}

// hopAddr is the comparison identity of a stored hop address: the parsed
// address when the text parses, the raw text when it does not.
type hopAddr struct {
	ip  netip.Addr
	raw string
}

func canonHopAddr(s string) hopAddr {
	if a, err := netip.ParseAddr(s); err == nil {
		return hopAddr{ip: a.Unmap().WithZone("")}
	}
	return hopAddr{raw: s}
}

// hopAddrSentinel replaces every non-empty hop address on /hops/timeline.
// It must not be "": ui/src/MtrHeatmap.tsx reads HopPoint.IP purely as a
// reply/no-reply flag (`p.IP ? lossColor(...) : noReply`), keyed separately
// by Index, and never displays the address itself — blanking to "" would
// make every hop render as no-reply and silently break the heatmap. It must
// also not be a real address, so any fixed, address-free string works; this
// one is chosen to be obviously not an IP if it ever leaks into a log or a
// debugger.
const hopAddrSentinel = "redacted"

// redactAllHopAddresses replaces every non-empty hop address with
// hopAddrSentinel, unlike redactTerminalHops which blanks only the
// apparent terminal row.
//
// queryHopsGrid returns one row per (slot, ttl), and its address is whichever
// responder the slot's worst-loss cycle saw — so a row's depth tracks no one
// trace, slots hold traces of differing length (path flaps are ordinary within
// a 15m bucket at a 1m probe interval, per T8), and a slot's deepest row can
// be a silent hop while the slave's own address sits above it. There is no
// per-row signal for "this is the terminal hop" left once cycles are
// aggregated — the only way to find it would be comparing rows against the
// real slave address, which is exactly the leak the Probe/Public split
// exists to prevent. So every row is redacted here, not just the row at the
// bucket's apparent max index: any positional heuristic can be defeated by
// an ordinary route change within the bucket. This is the fail-closed
// choice mandated for the timeline endpoint; /hops keeps terminal-only
// redaction because QueryLatestHops/QueryHopsAt pin one cycle per
// (source, time), so the terminal row is unambiguous there.
//
// Genuinely empty addresses (a hop that never replied) are left empty —
// see hopAddrSentinel for why that bit must survive unmolested. Annotations
// follow their address: every row loses Unreach and TargetReply here. The
// marker is cleared where redactTerminalHops keeps it because every address
// here is the same sentinel, so no row's stats set the target's apart and the
// marker alone would name the ttl the slave answered at; hopTimelineDTO has
// no field for it either way.
func redactAllHopAddresses(hops []storage.HopPoint) []storage.HopPoint {
	out := make([]storage.HopPoint, len(hops))
	copy(out, hops)
	for i := range out {
		if out[i].IP != "" {
			out[i].IP = hopAddrSentinel
		}
		out[i].Unreach = ""
		out[i].TargetReply = false
	}
	return out
}

// statusRecentCycles is how many trailing raw cycles /status returns, and the
// query window is derived from it (cycles × interval) so the handler never
// scans rows it is about to trim — a fixed 24h window decoded ~690× what it
// kept and each result held a 256-entry cycle-cache slot.
const statusRecentCycles = 50

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}
	to := time.Now()
	from := to.Add(-time.Duration(statusRecentCycles) * s.store.Current().Interval)
	points, err := s.reader.QueryCycles(r.Context(), ref, from, to, storage.QueryFilter{Source: r.URL.Query().Get("source")})
	if err != nil {
		s.writeQueryErr(w, "query status", err)
		return
	}
	if len(points) > statusRecentCycles {
		points = points[len(points)-statusRecentCycles:]
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
	for _, t := range s.healthTargets() {
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
		writeErr(w, http.StatusBadRequest, "invalid from: "+err.Error())
		return time.Time{}, time.Time{}, false
	}
	to, err := parseTimeParam(q.Get("to"), now, now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid to: "+err.Error())
		return time.Time{}, time.Time{}, false
	}
	if !to.After(from) {
		writeErr(w, http.StatusBadRequest, "to must be after from")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// parseTimeParam accepts RFC3339, a unix timestamp, or a relative duration
// like "-1h" (interpreted from `now`). Empty returns the default. Every branch
// is bounded to storage's addressable range before the caller can convert it:
// a finite unix second past that range fits an int64 and then wraps in
// UnixMilli, putting the instant storage sees in the opposite epoch direction.
func parseTimeParam(s string, def, now time.Time) (time.Time, error) {
	t, err := resolveTimeParam(s, def, now)
	if err != nil {
		return time.Time{}, err
	}
	if !storage.ValidQueryTime(t) {
		return time.Time{}, fmt.Errorf("timestamp %s is outside the storable range %s..%s",
			t.UTC().Format(time.RFC3339), storage.MinQueryTime.Format("2006"), storage.MaxQueryTime.Format("2006"))
	}
	return t, nil
}

func resolveTimeParam(s string, def, now time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	// A whole decimal integer is unix seconds whatever its sign; a leading
	// sign only starts a duration when something follows the digits, which is
	// what separates "-1" from "-1h". Deciding on the sign alone made every
	// signed unix second a 400, and reading the unsigned form as an instant
	// while reading "-0" as an offset would be two grammars for one syntax.
	if ts, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(ts, 0), nil
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
	return time.Time{}, fmt.Errorf("%q is not RFC3339, unix seconds, or a duration like -1h", s)
}

// Upper bounds for the "d"/"w" units below. time.Duration is int64 nanoseconds,
// so n*24h overflows past ~106751 days; cap well below that (~10 years) so a
// crafted "?from=-9223372036d" can't wrap to a bogus window. The sign is
// preserved (windows are relative offsets), so the guard is on magnitude.
const (
	maxRelDays  = 3660 // ~10 years
	maxRelWeeks = 530  // ~10 years
)

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
		if err != nil || n < -maxRelDays || n > maxRelDays {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	case strings.HasSuffix(s, "w"):
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "w"), 10, 64)
		if err != nil || n < -maxRelWeeks || n > maxRelWeeks {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("invalid duration %q", s)
}

// pickStep returns the toStartOfInterval width for a cycles query
// covering [from, to]. The window-derived path delegates to
// storage.PickCycleStep so the tier table stays single-sourced; this
// wrapper exists solely to apply the back-compat `?step=` override:
//
//	?step=raw|1h|1d → forces a specific tier
//	anything else   → derived from window width.
//
// ok is false when the override buckets finer than the tier the ladder derives
// for the window — raw outside the raw tier, 1h past the 1h tier — because a
// finer step multiplies the row count the ladder holds to ~500-1000 buckets
// (?step=1h at 365d is 24x the ladder). Each bound is the ladder's own tier
// rather than a second copy of its threshold, so widening a tier widens the
// override with it.
func pickStep(override string, from, to time.Time) (step time.Duration, ok bool) {
	derived := storage.PickCycleStep(to.Sub(from))
	switch override {
	case "raw":
		return 0, derived == 0
	case "1h":
		return time.Hour, derived <= time.Hour
	case "1d":
		return 24 * time.Hour, derived <= 24*time.Hour
	}
	return derived, true
}

// writeQueryErr maps a reader failure to a status. Overload is backpressure
// rather than a fault, so it gets 503 and a Retry-After: 502 would tell a
// client the upstream is broken when it is merely busy.
func (s *Server) writeQueryErr(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, storage.ErrOverloaded) {
		s.log.Warn("query rejected", "query", what, "err", err)
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusServiceUnavailable, "server busy, retry shortly")
		return
	}
	if errors.Is(err, storage.ErrHopsTruncated) {
		s.log.Warn("query refused", "query", what, "err", err)
		writeErr(w, http.StatusBadRequest, "too many hop rows for this window: narrow the range or filter by source")
		return
	}
	s.log.Warn(what, "err", err)
	writeErr(w, http.StatusBadGateway, "query failed")
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

// Unwrap lets http.ResponseController (and middleware like chi's Compress)
// reach the underlying ResponseWriter for Flush/Hijack support. Without it
// the wrapper chain stops here and those interfaces are silently lost.
func (s *statusWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
