package alert

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
)

// ActionDispatcher fans an Event out to every configured action referenced by
// the alert. Webhook and exec failures are logged but don't block other actions.
type ActionDispatcher struct {
	log    *slog.Logger
	store  *config.Store
	client *http.Client
}

func NewDispatcher(log *slog.Logger, store *config.Store) *ActionDispatcher {
	return &ActionDispatcher{
		log:    log,
		store:  store,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *ActionDispatcher) Dispatch(ctx context.Context, e Event) {
	// Only notify when an alert enters or leaves firing — pending transitions
	// are too noisy for operators and the state-change log line already
	// captures them for debugging.
	if e.Next != StateFiring && e.Prev != StateFiring {
		return
	}
	cfg := d.store.Current()
	for _, name := range e.Alert.Actions {
		action, ok := cfg.Actions[name]
		if !ok {
			d.log.Warn("alert action not found", "action", name, "alert", e.AlertName)
			continue
		}
		body, err := renderTemplate(action.Template, e)
		if err != nil {
			d.log.Warn("render template", "action", name, "err", err)
			continue
		}
		switch action.Type {
		case "webhook":
			d.webhook(ctx, action, body, e)
		case "discord":
			d.discord(ctx, action, body, e)
		case "exec":
			d.exec(ctx, name, action, body, e)
		case "log":
			d.log.Info("alert",
				"target", e.Target.ID(), "alert", e.AlertName,
				"state", e.Next, "body", body)
		default:
			d.log.Warn("unknown action type", "type", action.Type, "action", name)
		}
	}
}

func (d *ActionDispatcher) webhook(ctx context.Context, a config.Action, body string, e Event) {
	// source stays the cycle that drove the transition (unchanged for existing
	// consumers); sources is every source firing at dispatch time, which under
	// quorum is the set the decision was actually made on. Always an array so
	// a consumer can index it without a null check.
	sources := e.FiringSources
	if sources == nil {
		sources = []string{}
	}
	payload := map[string]any{
		"target":  e.Target.ID(),
		"source":  e.Cycle.Source,
		"sources": sources,
		"alert":   e.AlertName,
		"state":   string(e.Next),
		"prev":    string(e.Prev),
		"message": body,
		"time":    e.Time.Format(time.RFC3339),
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		d.log.Warn("webhook marshal payload", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(buf))
	if err != nil {
		d.log.Warn("webhook request", "err", safeHTTPError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		d.log.Warn("webhook deliver", "err", httpFailureCategory(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		d.log.Warn("webhook non-2xx", "status", resp.StatusCode)
	}
}

func (d *ActionDispatcher) exec(ctx context.Context, name string, a config.Action, body string, e Event) {
	if a.Command == "" {
		return
	}
	execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Split the command on whitespace. For complex pipelines operators should
	// wrap them in a shell script and reference that.
	parts := strings.Fields(a.Command)
	if len(parts) == 0 {
		return
	}
	cmd := exec.CommandContext(execCtx, parts[0], parts[1:]...)
	cmd.Stdin = strings.NewReader(body)
	cmd.Env = append(cmd.Environ(),
		fmt.Sprintf("ALERT_TARGET=%s", e.Target.ID()),
		fmt.Sprintf("ALERT_NAME=%s", e.AlertName),
		fmt.Sprintf("ALERT_STATE=%s", e.Next),
		fmt.Sprintf("ALERT_SOURCE=%s", e.Cycle.Source),
		fmt.Sprintf("ALERT_SOURCES=%s", strings.Join(e.FiringSources, ",")),
	)
	// a.Command was env-expanded from the raw config bytes and can embed
	// credentials, exec.Error quotes argv[0], and the command's own output is
	// unbounded free text — so the log carries the action name and a fixed
	// category only, like its webhook/discord siblings.
	if _, err := cmd.CombinedOutput(); err != nil {
		d.log.Warn("exec failed", "action", name, "err", execFailureCategory(execCtx, err))
	}
}

// execFailureCategory names why an exec action failed using only fixed text
// plus the numeric exit code, never anything derived from the command line or
// the process.
func execFailureCategory(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("exit %d", exitErr.ExitCode())
	}
	return "start failed"
}

// discord posts a Discord-flavored embed to a webhook URL. If the action's
// template is set its rendered output becomes the embed description; otherwise
// we build a default. When the cycle carries trace Hops (only icmp/mtr populate
// them) we append an MTR-style code block so operators can see the path in the
// alert itself.
func (d *ActionDispatcher) discord(ctx context.Context, a config.Action, body string, e Event) {
	desc := discordDescription(a.Template, body, e)

	fields := []map[string]any{
		{"name": "State", "value": fmt.Sprintf("%s → %s", e.Prev, e.Next), "inline": true},
		{"name": "Loss", "value": lossField(e.Cycle.LossCount, e.Cycle.Sent), "inline": true},
		{"name": "Median RTT", "value": rttField(e.Cycle.Summary.Median), "inline": true},
	}
	if name, value := sourcesField(e); value != "" {
		fields = append(fields, map[string]any{"name": name, "value": value, "inline": true})
	}

	embed := map[string]any{
		"title":       fmt.Sprintf("%s — %s", e.AlertName, e.Target.ID()),
		"description": desc,
		"color":       discordColor(e.Next),
		"timestamp":   e.Time.UTC().Format(time.RFC3339),
		"fields":      fields,
	}

	payload := map[string]any{"embeds": []any{embed}}
	buf, err := json.Marshal(payload)
	if err != nil {
		d.log.Warn("discord marshal payload", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(buf))
	if err != nil {
		d.log.Warn("discord request", "err", safeHTTPError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		d.log.Warn("discord deliver", "err", httpFailureCategory(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		d.log.Warn("discord non-2xx", "status", resp.StatusCode)
	}
}

// URL errors can quote credential-bearing paths, malformed authorities, or
// redirect targets. A fixed category avoids trusting nested cause text.
const safeHTTPError = "request failed"

// httpFailureCategory names why a delivery failed using only the constants
// below, never text derived from the error: a Discord webhook URL is itself
// the credential, and url.Error quotes the URL at every nesting level.
func httpFailureCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "tls"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return safeHTTPError
}

func discordDescription(tmpl, body string, e Event) string {
	var buf bytes.Buffer
	if tmpl != "" {
		buf.WriteString(body)
	} else {
		fmt.Fprintf(&buf, "`%s` is **%s** (was %s).", e.Alert.Condition, e.Next, e.Prev)
	}
	if len(e.Cycle.Hops) > 0 {
		buf.WriteString("\n\n**Path**\n```\n")
		buf.WriteString(formatHops(e.Cycle.Hops))
		buf.WriteString("```")
	}
	// Discord embed descriptions are capped at 4096 chars; truncate on a valid
	// UTF-8 boundary to avoid sending a malformed string.
	const maxDesc = 4096
	s := buf.String()
	if len(s) > maxDesc {
		n := maxDesc
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return s[:n]
	}
	return s
}

// sourcesField renders the Discord field naming who is firing: every source
// under quorum, where the triggering cycle is only one of them. Falls back to
// the triggering cycle's source when nothing is firing — a full resolve, where
// the set is empty by construction. Returns an empty value when there is
// no source to name at all (standalone node), so the caller drops the field.
//
// Discord caps a field value at 1024 chars; a large mesh can exceed that with
// names alone, and an over-long field fails the whole embed with a 400, so the
// list is truncated rather than sent whole.
func sourcesField(e Event) (name, value string) {
	if len(e.FiringSources) == 0 {
		return "Source", e.Cycle.Source
	}
	if len(e.FiringSources) == 1 {
		return "Source", e.FiringSources[0]
	}
	const maxValue = 1024
	var buf strings.Builder
	for i, src := range e.FiringSources {
		sep := ""
		if i > 0 {
			sep = ", "
		}
		more := fmt.Sprintf(" +%d more", len(e.FiringSources)-i)
		if buf.Len()+len(sep)+len(src)+len(more) > maxValue {
			buf.WriteString(more)
			break
		}
		buf.WriteString(sep)
		buf.WriteString(src)
	}
	return fmt.Sprintf("Sources (%d)", len(e.FiringSources)), buf.String()
}

func discordColor(s State) int {
	switch s {
	case StateFiring:
		return 0xE53935 // red
	case StatePending:
		return 0xFB8C00 // orange
	case StateOK:
		return 0x43A047 // green
	}
	return 0x757575
}

func lossField(lost, sent int) string {
	if sent == 0 {
		return "—"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", lost, sent, 100*float64(lost)/float64(sent))
}

func rttField(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	return d.Round(100 * time.Microsecond).String()
}

// formatHops renders hops as a fixed-width table suited for a Discord code
// block. Unresponsive hops (empty IP) render as "*", matching the trace output
// convention. An unreachable label goes in a trailing column rather than the
// host cell, which truncates at ipCol and would cut it mid-word.
func formatHops(hops []probe.Hop) string {
	const ipCol = 17
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%-3s %-*s %6s %8s\n", "#", ipCol, "HOST", "LOSS%", "AVG")
	for _, h := range hops {
		ip := h.IP
		if ip == "" {
			ip = "*"
		}
		if len(ip) > ipCol {
			ip = ip[:ipCol]
		}
		loss := "—"
		if h.Sent > 0 {
			loss = fmt.Sprintf("%3.0f%%", 100*float64(h.Lost)/float64(h.Sent))
		}
		avg := "—"
		if n := len(h.RTTs); n > 0 {
			var sum time.Duration
			for _, r := range h.RTTs {
				sum += r
			}
			avg = (sum / time.Duration(n)).Round(100 * time.Microsecond).String()
		}
		fmt.Fprintf(&buf, "%-3d %-*s %6s %8s", h.Index, ipCol, ip, loss, avg)
		if h.Unreach != "" {
			fmt.Fprintf(&buf, "  !%s", h.Unreach)
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

func renderTemplate(tmpl string, e Event) (string, error) {
	if tmpl == "" {
		return fmt.Sprintf("%s: %s → %s", e.Target.ID(), e.AlertName, e.Next), nil
	}
	t, err := template.New("alert").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, e); err != nil {
		return "", err
	}
	return buf.String(), nil
}
