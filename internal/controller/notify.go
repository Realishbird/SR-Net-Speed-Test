package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Phase 7: notifications. Channels are Slack or Microsoft Teams incoming
// webhooks, or a plain JSON webhook for anything else (PagerDuty, Mattermost,
// a ticket system, your own script). Each channel picks the events it wants.
// Delivery is asynchronous with three attempts; the last outcome per channel
// is shown on the Settings page.

// NotifyChannel is one destination.
type NotifyChannel struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"` // slack | teams | webhook
	URL     string   `json:"url"`
	Secret  string   `json:"secret,omitempty"` // webhook: HMAC-SHA256 of the body in X-SRNet-Signature
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`

	// Status, filled by the notifier, not stored.
	LastAt    *time.Time `json:"last_at,omitempty"`
	LastOK    bool       `json:"last_ok,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	Sent      int        `json:"sent,omitempty"`
}

// NotifySettings is the "notify" setting.
type NotifySettings struct {
	Channels []NotifyChannel `json:"channels"`
}

// notifyEvents are the events a channel can subscribe to, with the label
// the Settings page shows.
var notifyEvents = []struct{ Key, Label string }{
	{"alert_open", "Alert opened (link down, hot optic, errors while idle, agent offline, fabric mismatch, run failed)"},
	{"alert_resolve", "Alert resolved"},
	{"run_pass", "Run passed"},
	{"run_warn", "Run passed with warnings (error counters grew, leg behind its baseline)"},
	{"run_fail", "Run failed or errored"},
	{"suite_done", "Suite run finished (nightly acceptance)"},
	{"update_done", "Fleet update finished"},
	{"host_pending", "Host enrolled and waiting for approval"},
}

// notifyMsg is what every channel kind renders in its own shape.
type notifyMsg struct {
	Event  string
	Level  string // ok | warn | fail | info
	Title  string
	Text   string
	Link   string
	Fields [][2]string
	Data   any
}

type notifier struct {
	store *Store
	base  string // controller public URL for links
	httpc *http.Client
	mu    sync.Mutex
	chans []NotifyChannel
	stat  map[int64]*NotifyChannel                  // status per channel id
	sent  func(ch NotifyChannel, body []byte) error // test hook
}

func newNotifier(s *Store, base string) *notifier {
	n := &notifier{store: s, base: strings.TrimRight(base, "/"), httpc: &http.Client{Timeout: 10 * time.Second}, stat: map[int64]*NotifyChannel{}}
	n.sent = n.post
	return n
}

func (n *notifier) load(ctx context.Context) {
	if n.store == nil {
		return
	}
	var ns NotifySettings
	if _, err := n.store.Setting(ctx, "notify", &ns); err != nil {
		log.Printf("notify: setting: %v", err)
		return
	}
	n.mu.Lock()
	n.chans = ns.Channels
	n.mu.Unlock()
}

func (n *notifier) channels() []NotifyChannel {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]NotifyChannel, len(n.chans))
	for i, c := range n.chans {
		out[i] = c
		if st := n.stat[c.ID]; st != nil {
			out[i].LastAt, out[i].LastOK, out[i].LastError, out[i].Sent = st.LastAt, st.LastOK, st.LastError, st.Sent
		}
	}
	return out
}

func (n *notifier) save(ctx context.Context, chans []NotifyChannel) error {
	next := int64(1)
	for _, c := range chans {
		if c.ID >= next {
			next = c.ID + 1
		}
	}
	for i := range chans {
		c := &chans[i]
		c.Name = strings.TrimSpace(c.Name)
		c.URL = strings.TrimSpace(c.URL)
		if c.ID == 0 {
			c.ID = next
			next++
		}
		if c.Name == "" {
			return &userError{fmt.Errorf("channel %d has no name", i+1)}
		}
		switch c.Kind {
		case "slack", "teams", "webhook":
		default:
			return &userError{fmt.Errorf("%s: kind must be slack, teams or webhook", c.Name)}
		}
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return &userError{fmt.Errorf("%s: the URL must start with https://", c.Name)}
		}
		if c.Events == nil {
			c.Events = []string{}
		}
		c.LastAt, c.LastOK, c.LastError, c.Sent = nil, false, "", 0
	}
	if n.store != nil {
		if err := n.store.SaveSetting(ctx, "notify", NotifySettings{Channels: chans}); err != nil {
			return err
		}
	}
	n.mu.Lock()
	n.chans = chans
	n.mu.Unlock()
	return nil
}

// send fans a message out to every enabled channel subscribed to its event.
func (n *notifier) send(m notifyMsg) {
	if n == nil {
		return
	}
	for _, c := range n.channels() {
		if !c.Enabled || !contains(c.Events, m.Event) {
			continue
		}
		go n.deliver(c, m)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// deliver renders and posts with three attempts, recording the outcome.
func (n *notifier) deliver(c NotifyChannel, m notifyMsg) error {
	body := renderNotify(c.Kind, m)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(2<<attempt) * time.Second)
		}
		if err = n.sent(c, body); err == nil {
			break
		}
	}
	now := time.Now()
	n.mu.Lock()
	st := n.stat[c.ID]
	if st == nil {
		st = &NotifyChannel{}
		n.stat[c.ID] = st
	}
	st.LastAt, st.LastOK = &now, err == nil
	st.LastError = ""
	if err != nil {
		st.LastError = err.Error()
		log.Printf("notify %s (%s): %v", c.Name, c.Kind, err)
	} else {
		st.Sent++
	}
	n.mu.Unlock()
	return err
}

func (n *notifier) post(c NotifyChannel, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "srnet-controller")
	if c.Kind == "webhook" && c.Secret != "" {
		mac := hmac.New(sha256.New, []byte(c.Secret))
		mac.Write(body)
		req.Header.Set("X-SRNet-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := n.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var b [200]byte
		k, _ := resp.Body.Read(b[:])
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b[:k])))
	}
	return nil
}

// renderNotify shapes a message for each channel kind.
func renderNotify(kind string, m notifyMsg) []byte {
	colors := map[string]string{"ok": "#1baf7a", "warn": "#eda100", "fail": "#e34948", "info": "#2a78d6"}
	color := colors[m.Level]
	if color == "" {
		color = colors["info"]
	}
	var out any
	switch kind {
	case "slack":
		fields := make([]map[string]any, 0, len(m.Fields))
		for _, f := range m.Fields {
			fields = append(fields, map[string]any{"title": f[0], "value": f[1], "short": true})
		}
		title := m.Title
		if m.Link != "" {
			title = "<" + m.Link + "|" + m.Title + ">"
		}
		out = map[string]any{"text": title, "attachments": []map[string]any{{"color": color, "text": m.Text, "fields": fields, "mrkdwn_in": []string{"text"}}}}
	case "teams":
		facts := make([]map[string]string, 0, len(m.Fields))
		for _, f := range m.Fields {
			facts = append(facts, map[string]string{"title": f[0], "value": f[1]})
		}
		body := []map[string]any{{"type": "TextBlock", "size": "Medium", "weight": "Bolder", "text": m.Title, "wrap": true},
			{"type": "TextBlock", "text": m.Text, "wrap": true}}
		if len(facts) > 0 {
			body = append(body, map[string]any{"type": "FactSet", "facts": facts})
		}
		card := map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4", "body": body}
		if m.Link != "" {
			card["actions"] = []map[string]any{{"type": "Action.OpenUrl", "title": "Open in SR-Net", "url": m.Link}}
		}
		out = map[string]any{"type": "message", "attachments": []map[string]any{{"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil, "content": card}}}
	default:
		fields := map[string]string{}
		for _, f := range m.Fields {
			fields[f[0]] = f[1]
		}
		out = map[string]any{"event": m.Event, "level": m.Level, "title": m.Title, "text": m.Text, "link": m.Link, "fields": fields, "data": m.Data, "time": time.Now()}
	}
	b, _ := json.Marshal(out)
	return b
}

// Events.

func (n *notifier) link(path string) string {
	if n == nil || n.base == "" {
		return ""
	}
	return n.base + "/" + path
}

func (n *notifier) alert(state string, al api.Alert) {
	if n == nil {
		return
	}
	ev, level, title := "alert_open", "fail", "Alert: "+al.Kind
	if state == "resolved" {
		ev, level, title = "alert_resolve", "ok", "Resolved: "+al.Kind
	}
	where := strings.TrimSpace(al.Host + " " + al.Port)
	text := al.Message
	if state == "resolved" && text == "" {
		text = where + " is back within limits"
	}
	n.send(notifyMsg{Event: ev, Level: level, Title: title, Text: text, Link: n.link("#alerts"),
		Fields: [][2]string{{"Host", nonEmpty(al.Host, "-")}, {"Port", nonEmpty(al.Port, "-")}}, Data: al})
}

// runFinished is the orchestrator's hook for every run that ends.
func (n *notifier) runFinished(r api.Run) {
	if n == nil {
		return
	}
	ev, level := "run_pass", "ok"
	switch {
	case r.Status != "done" || r.Verdict == "fail":
		ev, level = "run_fail", "fail"
	case r.Verdict == "warn":
		ev, level = "run_warn", "warn"
	}
	title := fmt.Sprintf("Run %d %s: %s", r.ID, strings.ToUpper(nonEmpty(r.Verdict, r.Status)), r.Name)
	var notes []string
	for _, l := range r.Legs {
		if l.Verdict != "pass" && l.Note != "" {
			notes = append(notes, fmt.Sprintf("%s > %s: %s", l.ClientHost, l.ServerHost, l.Note))
		}
	}
	sort.Strings(notes)
	if len(notes) > 6 {
		notes = append(notes[:6], fmt.Sprintf("and %d more", len(notes)-6))
	}
	text := fmt.Sprintf("%s, %d legs, %.1f Gbps aggregate", r.Spec.Test, len(r.Legs), r.Summary.ToolGbps)
	if r.Error != "" {
		text += ". " + r.Error
	}
	if len(notes) > 0 {
		text += "\n" + strings.Join(notes, "\n")
	}
	fields := [][2]string{{"Test", r.Spec.Test}, {"Aggregate", fmt.Sprintf("%.1f Gbps", r.Summary.ToolGbps)}}
	if r.Summary.BaselineRunID > 0 {
		fields = append(fields, [2]string{"vs baseline", fmt.Sprintf("%+.1f%% (run %d)", r.Summary.DeltaPct, r.Summary.BaselineRunID)})
	}
	n.send(notifyMsg{Event: ev, Level: level, Title: title, Text: text, Link: n.link(fmt.Sprintf("#run/%d", r.ID)), Fields: fields,
		Data: map[string]any{"run_id": r.ID, "name": r.Name, "status": r.Status, "verdict": r.Verdict, "summary": r.Summary, "spec": r.Spec}})
}

func (n *notifier) suiteFinished(sr api.SuiteRun) {
	if n == nil {
		return
	}
	level := "ok"
	if sr.Status != "done" || sr.Verdict == "fail" {
		level = "fail"
	} else if sr.Verdict == "warn" {
		level = "warn"
	}
	ids := make([]string, len(sr.RunIDs))
	for i, id := range sr.RunIDs {
		ids[i] = fmt.Sprint(id)
	}
	n.send(notifyMsg{Event: "suite_done", Level: level, Title: fmt.Sprintf("Suite %s: %s", sr.SuiteName, strings.ToUpper(nonEmpty(sr.Verdict, sr.Status))),
		Text: fmt.Sprintf("%d runs (%s), started by %s", len(sr.RunIDs), strings.Join(ids, ", "), sr.Trigger),
		Link: n.link(fmt.Sprintf("api/v1/suite-runs/%d/report.html", sr.ID)), Fields: [][2]string{{"Suite", sr.SuiteName}, {"Runs", fmt.Sprint(len(sr.RunIDs))}}, Data: sr})
}

func (n *notifier) updateFinished(job api.UpdateJob) {
	if n == nil {
		return
	}
	done, failed, skipped := 0, 0, 0
	for _, h := range job.Hosts {
		switch h.Status {
		case "done":
			done++
		case "failed":
			failed++
		default:
			skipped++
		}
	}
	level := "ok"
	if failed > 0 {
		level = "fail"
	}
	n.send(notifyMsg{Event: "update_done", Level: level, Title: fmt.Sprintf("Fleet update to %s: %s", job.Version, job.Status),
		Text: fmt.Sprintf("%d updated, %d failed, %d skipped", done, failed, skipped), Link: n.link("#fleet"),
		Fields: [][2]string{{"Version", job.Version}, {"Updated", fmt.Sprint(done)}, {"Failed", fmt.Sprint(failed)}}, Data: job})
}

func (n *notifier) hostPending(name, ip string) {
	if n == nil {
		return
	}
	n.send(notifyMsg{Event: "host_pending", Level: "info", Title: "Host waiting for approval: " + name,
		Text: fmt.Sprintf("%s (%s) enrolled from an allowed subnet without a token and waits on the Hosts page", name, ip), Link: n.link("#hosts"),
		Fields: [][2]string{{"Host", name}, {"IP", ip}}})
}

// Routes.

func (s *Server) routesNotify(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/notify", s.admin(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"channels": s.notify.channels(), "events": notifyEvents})
	}))
	mux.HandleFunc("PUT /api/v1/notify", s.admin(func(w http.ResponseWriter, r *http.Request) {
		var ns NotifySettings
		if !readJSON(w, r, &ns) {
			return
		}
		if ns.Channels == nil {
			ns.Channels = []NotifyChannel{}
		}
		if err := s.notify.save(r.Context(), ns.Channels); err != nil {
			var ue *userError
			if errors.As(err, &ue) {
				httpError(w, http.StatusBadRequest, err.Error())
			} else {
				serverError(w, err)
			}
			return
		}
		s.store.Audit(r.Context(), "notify.save", map[string]any{"channels": len(ns.Channels), "by": who(r).Name})
		writeJSON(w, map[string]any{"channels": s.notify.channels(), "events": notifyEvents})
	}))
	mux.HandleFunc("POST /api/v1/notify/{id}/test", s.admin(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		for _, c := range s.notify.channels() {
			if c.ID == id {
				err := s.notify.deliver(c, notifyMsg{Event: "test", Level: "info", Title: "SR-Net test message",
					Text: "Sent from the Settings page by " + who(r).Name + ". If you can read this, the channel works.", Link: s.notify.link("#alerts"),
					Fields: [][2]string{{"Controller", s.cfg.PublicURL}}})
				if err != nil {
					httpError(w, http.StatusBadGateway, err.Error())
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		httpError(w, http.StatusNotFound, "no such channel")
	}))
}
