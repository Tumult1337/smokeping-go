package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
)

// ActionDispatcher fans an Event out to every configured action referenced by
// the alert. Webhook and exec failures are logged but don't block other actions.
type ActionDispatcher struct {
	log     *slog.Logger
	store   *config.Store
	client  *http.Client
}

func NewDispatcher(log *slog.Logger, store *config.Store) *ActionDispatcher {
	return &ActionDispatcher{
		log:    log,
		store:  store,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *ActionDispatcher) Dispatch(ctx context.Context, e Event) {
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
			d.exec(ctx, action, body, e)
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
	payload := map[string]any{
		"target":  e.Target.ID(),
		"alert":   e.AlertName,
		"state":   string(e.Next),
		"prev":    string(e.Prev),
		"message": body,
		"time":    e.Time.Format(time.RFC3339),
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(buf))
	if err != nil {
		d.log.Warn("webhook request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		d.log.Warn("webhook deliver", "url", a.URL, "err", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		d.log.Warn("webhook non-2xx", "url", a.URL, "status", resp.StatusCode)
	}
}

func (d *ActionDispatcher) exec(ctx context.Context, a config.Action, body string, e Event) {
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
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		d.log.Warn("exec", "command", a.Command, "err", err, "output", string(out))
	}
}

// discord posts a Discord-flavored embed to a webhook URL. If the action's
// template is set its rendered output becomes the embed description; otherwise
// we build a default. When the cycle carries trace Hops (only icmp/mtr populate
// them) we append an MTR-style code block so operators can see the path in the
// alert itself.
func (d *ActionDispatcher) discord(ctx context.Context, a config.Action, body string, e Event) {
	desc := discordDescription(a.Template, body, e)

	embed := map[string]any{
		"title":       fmt.Sprintf("%s — %s", e.AlertName, e.Target.ID()),
		"description": desc,
		"color":       discordColor(e.Next),
		"timestamp":   e.Time.UTC().Format(time.RFC3339),
		"fields": []map[string]any{
			{"name": "State", "value": fmt.Sprintf("%s → %s", e.Prev, e.Next), "inline": true},
			{"name": "Loss", "value": lossField(e.Cycle.LossCount, e.Cycle.Sent), "inline": true},
			{"name": "Median RTT", "value": rttField(e.Cycle.Summary.Median), "inline": true},
		},
	}

	payload := map[string]any{"embeds": []any{embed}}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(buf))
	if err != nil {
		d.log.Warn("discord request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		d.log.Warn("discord deliver", "url", a.URL, "err", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		d.log.Warn("discord non-2xx", "url", a.URL, "status", resp.StatusCode)
	}
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
	// Discord embed descriptions are capped at 4096 chars.
	const maxDesc = 4096
	if buf.Len() > maxDesc {
		return buf.String()[:maxDesc]
	}
	return buf.String()
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
// convention.
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
		fmt.Fprintf(&buf, "%-3d %-*s %6s %8s\n", h.Index, ipCol, ip, loss, avg)
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
