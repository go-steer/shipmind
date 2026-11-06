// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scenario

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Router talks to an agent's session API (mast's attach surface)
// through Substrate's router, which wakes a suspended task on the way.
type Router struct {
	// Base is the router's URL ("http://atenet-router.ate-system.svc.cluster.local").
	Base string
	// Atespace is the agents' ax atespace.
	Atespace string
	// Token is the agent's attach token.
	Token string
	// Client defaults to one with a timeout long enough for a cold wake.
	Client *http.Client
	// EventsWait bounds how long Events reads the event stream, which
	// replays the session and then stays open for new events.
	EventsWait time.Duration
	// MaxRaw bounds how much of the stream Replay keeps (default 64 MiB).
	MaxRaw int64
}

// Session is one row of GET /sessions.
type Session struct {
	ID string `json:"sessionID"`
	// Status is "active" for a session in mast's memory and "idle" for
	// one known only from its store (after a restart, before it is used).
	Status string `json:"status"`
}

// routerTimeout is long enough for the router to wake a suspended task.
const routerTimeout = 120 * time.Second

func (r *Router) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: routerTimeout}
}

func (r *Router) get(ctx context.Context, task, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(r.Base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("ate-target-actor", r.Atespace+"/"+task)
	req.Header.Set("Authorization", "Bearer "+r.Token)
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, &statusError{code: resp.StatusCode, msg: fmt.Sprintf("GET %s on %s: %s: %s", path, task, resp.Status, strings.TrimSpace(string(b)))}
	}
	return resp, nil
}

// statusError is a non-200 answer from the agent.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func (r *Router) getJSON(ctx context.Context, task, path string, v any) error {
	resp, err := r.get(ctx, task, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Sessions lists a task's sessions.
func (r *Router) Sessions(ctx context.Context, task string) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	if err := r.getJSON(ctx, task, "/sessions", &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// State returns a session's agent state ("idle" when not working).
//
// core-agent reports "idle" between turns even while a subagent the
// model started in the background (spawn_agent) is still investigating;
// its result arrives later and starts another turn. So an idle session
// with a running subagent is reported as "working". mast has no
// /agents route, and its 404 means none.
func (r *Router) State(ctx context.Context, task, session string) (string, error) {
	var out struct {
		State string `json:"state"`
	}
	if err := r.getJSON(ctx, task, "/sessions/"+url.PathEscape(session)+"/status", &out); err != nil {
		return "", err
	}
	if out.State != "idle" {
		return out.State, nil
	}
	var agents struct {
		Agents []struct {
			Status string `json:"status"`
		} `json:"agents"`
	}
	err := r.getJSON(ctx, task, "/sessions/"+url.PathEscape(session)+"/agents", &agents)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusNotFound:
		return out.State, nil
	case err != nil:
		return "", err
	}
	for _, a := range agents.Agents {
		if a.Status == "running" {
			return "working", nil
		}
	}
	return out.State, nil
}

// AgentUsage is GET /sessions/{id}/usage as the agent answers it (mast
// and core-agent share the shape, not every meaning): session totals,
// and one row per model call ("turn" in their naming).
type AgentUsage struct {
	Overall struct {
		InputTokens   int64   `json:"input_tokens"`
		CachedTokens  int64   `json:"input_tokens_cached"`
		OutputTokens  int64   `json:"output_tokens"`
		ThoughtTokens int64   `json:"thoughts_tokens"`
		Calls         int     `json:"turns"`
		CostUSD       float64 `json:"cost_usd"`
	} `json:"overall"`
	PerTurn []struct {
		Call          int     `json:"turn"`
		At            string  `json:"ts"`
		Model         string  `json:"model"`
		InputTokens   int64   `json:"input_tokens"`
		CachedTokens  int64   `json:"input_tokens_cached"`
		OutputTokens  int64   `json:"output_tokens"`
		ThoughtTokens int64   `json:"thoughts_tokens"`
		CostUSD       float64 `json:"cost_usd"`
	} `json:"per_turn"`
}

// Usage reads a session's model usage as the agent reports it.
func (r *Router) Usage(ctx context.Context, task, session string) (*AgentUsage, error) {
	var u AgentUsage
	if err := r.getJSON(ctx, task, "/sessions/"+url.PathEscape(session)+"/usage", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Usage is a session's model usage in one meaning whichever agent
// reported it. The totals are the session's when they were read (at
// ReadAt, once the answer settled), so they include every call made
// for the incident so far: if a later message arrived first
// (Result.Superseded), calls for it may be in too; Calls carries each
// call's time to tell.
type Usage struct {
	// Agent is the server the session ran on ("core-agent/<version>",
	// "mast/<version>"), from its event stream.
	Agent  string `json:"agent,omitempty"`
	ReadAt string `json:"read_at"`

	ModelCalls int `json:"model_calls"`
	// InputTokens includes CachedInputTokens, the part served from the
	// prompt cache.
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	// OutputTokens excludes ThinkingTokens (mast reports them inside
	// its output count; core-agent beside it).
	OutputTokens   int64 `json:"output_tokens"`
	ThinkingTokens int64 `json:"thinking_tokens"`
	// CostUSD is the agent's own estimate. On mast it can include spend
	// carried over from an earlier process of the same session; the
	// result's top-level cost_usd is the event stream's figure.
	CostUSD float64 `json:"cost_usd"`

	// Calls is one row per model call, as far back as the agent keeps
	// them (mast: its last 500).
	Calls []UsageCall `json:"calls,omitempty"`
}

// UsageCall is one model call, in Usage's meanings.
type UsageCall struct {
	Call              int     `json:"call"`
	At                string  `json:"at,omitempty"`
	Model             string  `json:"model,omitempty"`
	InputTokens       int64   `json:"input_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ThinkingTokens    int64   `json:"thinking_tokens"`
	CostUSD           float64 `json:"cost_usd"`
}

// Normalize puts u in Usage's meanings for the agent server: mast counts
// thinking inside output, so it is taken out there.
func (u *AgentUsage) Normalize(server string, readAt time.Time) *Usage {
	thinkingInOutput := strings.HasPrefix(server, "mast/")
	output := func(out, thinking int64) int64 {
		if thinkingInOutput {
			return max(out-thinking, 0)
		}
		return out
	}
	o := u.Overall
	n := &Usage{
		Agent:             server,
		ReadAt:            readAt.UTC().Format(time.RFC3339),
		ModelCalls:        o.Calls,
		InputTokens:       o.InputTokens,
		CachedInputTokens: o.CachedTokens,
		OutputTokens:      output(o.OutputTokens, o.ThoughtTokens),
		ThinkingTokens:    o.ThoughtTokens,
		CostUSD:           o.CostUSD,
	}
	for _, c := range u.PerTurn {
		n.Calls = append(n.Calls, UsageCall{
			Call: c.Call, At: c.At, Model: c.Model,
			InputTokens: c.InputTokens, CachedInputTokens: c.CachedTokens,
			OutputTokens: output(c.OutputTokens, c.ThoughtTokens), ThinkingTokens: c.ThoughtTokens,
			CostUSD: c.CostUSD,
		})
	}
	return n
}

// Events reads a session's event stream for EventsWait (default 15s).
func (r *Router) Events(ctx context.Context, task, session string) (*Transcript, error) {
	wait := r.EventsWait
	if wait == 0 {
		wait = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := r.get(ctx, task, "/sessions/"+url.PathEscape(session)+"/events")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	t, err := ParseEvents(resp.Body)
	// The stream doesn't end on its own; running out of time is how a
	// read finishes.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, err
	}
	return t, nil
}

// Replay reads a session's stream as it comes, for the run's archive:
// every frame, up to MaxRaw bytes (default 64 MiB), kept even when it
// doesn't parse. core-agent replays a session's newest 5000 events.
func (r *Router) Replay(ctx context.Context, task, session string) (*Replay, error) {
	wait := r.EventsWait
	if wait == 0 {
		wait = 15 * time.Second
	}
	limit := r.MaxRaw
	if limit == 0 {
		limit = 64 << 20
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := r.get(ctx, task, "/sessions/"+url.PathEscape(session)+"/events")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, err
	}
	rp := &Replay{Raw: raw}
	if int64(len(raw)) > limit {
		rp.Raw, rp.Truncated = raw[:limit], true
	}
	if t, _ := ParseEvents(bytes.NewReader(rp.Raw)); t != nil {
		rp.Server = t.Server
	}
	return rp, nil
}

// Replay is a session's stream as read for the archive.
type Replay struct {
	Raw []byte
	// Server is the agent, from the stream's capabilities frame.
	Server string
	// Truncated: the stream ran past the read's limit.
	Truncated bool
}

// Transcript is a session's events, as far as scoring needs them.
type Transcript struct {
	// Server is the agent the stream came from, from its capabilities
	// frame ("core-agent/<version>", "mast/<version>").
	Server string
	Events []Event
	// Cost is the session's running cost in US dollars, from the last
	// usage update.
	Cost float64
}

// Event is one agent event: who wrote it and its text parts.
type Event struct {
	Seq    int
	Author string
	Role   string
	Texts  []string
}

// ParseEvents reads the agent's server-sent events (mast's and
// core-agent's share the shape): "data:" lines carrying an
// object with a seq are session events; one carrying cost_usd_total is a
// usage update. Other lines are ignored. It returns what it parsed when
// the stream fails, along with the error.
func ParseEvents(r io.Reader) (*Transcript, error) {
	t := &Transcript{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var d struct {
			Seq   *int `json:"seq"`
			Event struct {
				Author  string `json:"author"`
				Content *struct {
					Role  string `json:"role"`
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"event"`
			Cost   *float64 `json:"cost_usd_total"`
			Server string   `json:"server"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			continue
		}
		if d.Cost != nil {
			t.Cost = *d.Cost
		}
		if d.Server != "" {
			t.Server = d.Server
		}
		if d.Seq == nil {
			continue
		}
		e := Event{Seq: *d.Seq, Author: d.Event.Author}
		if c := d.Event.Content; c != nil {
			e.Role = c.Role
			for _, p := range c.Parts {
				if p.Text != "" {
					e.Texts = append(e.Texts, p.Text)
				}
			}
		}
		t.Events = append(t.Events, e)
	}
	return t, sc.Err()
}

// isUser reports whether e is a message sent to the agent: authored by
// "user" and carrying text. Tool results carry role "user" too, from the
// specialist that called the tool, and function responses come from
// "user" with no text; neither is a new message.
//
// A core-agent "[Background reports]" turn isn't one either: it carries
// a background subagent's result back to the agent that started it, and
// the agent's answer comes after it.
func isUser(e Event) bool {
	return e.Author == "user" && len(e.Texts) > 0 && !strings.HasPrefix(e.Texts[0], backgroundReports)
}

// backgroundReports heads the turn core-agent delivers background
// subagent results in.
const backgroundReports = "[Background reports]"

// inboxHeader heads every message core-agent's daemon queues for a turn
// ("[Inbox]\n- from <sender>: <message>\n...\n\nHow to handle the
// bundle: ..."); mast stores the message as sent.
const inboxHeader = "[Inbox]\n- "

// messageText returns the message a person or lookout sent, from the
// text of the event that carries it: core-agent's first inbox bullet
// without its sender, or mast's text as is. What follows the message in
// an inbox bundle (more bullets, core-agent's guidance) is left on; a
// caller decoding JSON reads only the first value.
func messageText(text string) string {
	rest, ok := strings.CutPrefix(text, inboxHeader)
	if !ok {
		return text
	}
	// "from <sender>: ", where a sender (a caller identity) has no
	// whitespace; an unattributed message has no prefix.
	if head, after, ok := strings.Cut(rest, ": "); ok {
		if sender, ok := strings.CutPrefix(head, "from "); ok && sender != "" && !strings.ContainsAny(sender, " \t\n") {
			rest = after
		}
	}
	return rest
}

// Answer returns the agent's answer to the first message a person or
// lookout sent: the coordinator's last text before the next such message
// that follows it (lookout re-sends an incident while it stays broken,
// and the agent answers a repeat with a one-line acknowledgement, which
// must not count as the diagnosis). A message arriving before any answer
// joins the same turn: lookout sends updates about the incident while
// the agent is still investigating, and the answer then covers both.
// Without an author ending in "_coordinator", the last non-user text
// counts. It also returns whether a later message arrived after the
// answer, meaning the answer is complete.
func (t *Transcript) Answer() (answer string, superseded bool) {
	first := -1
	hasCoordinator := false
	for i, e := range t.Events {
		if first < 0 && isUser(e) {
			first = i
		}
		if strings.HasSuffix(e.Author, "_coordinator") {
			hasCoordinator = true
		}
	}
	if first < 0 {
		return "", false
	}
	var coordinator, other string
	for _, e := range t.Events[first+1:] {
		if isUser(e) {
			if coordinator != "" || (!hasCoordinator && other != "") {
				superseded = true
				break
			}
			continue
		}
		if len(e.Texts) == 0 || e.Author == "user" || e.Role == "user" {
			continue
		}
		text := strings.Join(e.Texts, "\n")
		if strings.HasSuffix(e.Author, "_coordinator") {
			coordinator = text
		}
		other = text
	}
	if coordinator != "" {
		return coordinator, superseded
	}
	return other, superseded
}

// Mentions reports whether any event's text contains s, for finding the
// session an incident landed in.
func (t *Transcript) Mentions(s string) bool {
	for _, e := range t.Events {
		for _, text := range e.Texts {
			if strings.Contains(text, s) {
				return true
			}
		}
	}
	return false
}

// runNameSuffix matches what RunName appends: -MMDD-HHMMSS.
var runNameSuffix = regexp.MustCompile(`^-?\d{4}-\d{6}`)

// mentionsRun reports whether text names a run workload of a scenario,
// given prefix "<scenario>-": the prefix followed by a run timestamp.
func mentionsRun(text, prefix string) bool {
	for rest := text; ; {
		i := strings.Index(rest, prefix)
		if i < 0 {
			return false
		}
		rest = rest[i+len(prefix):]
		if runNameSuffix.MatchString(rest) {
			return true
		}
	}
}

// FirstMessage returns the first message sent to the agent: for a
// lookout incident task, the incident itself.
func (t *Transcript) FirstMessage() string {
	for _, e := range t.Events {
		if isUser(e) {
			return messageText(strings.Join(e.Texts, "\n"))
		}
	}
	return ""
}

// Incident is the identity of the incident lookout opened a task for,
// read from the task's first message (lookout's inject payload).
type Incident struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// FirstIncident parses the task's first message as a lookout incident.
// ok is false when it isn't one; a storm (kind "storm") never is: a
// storm task is keyed on a shared ancestor (a Node, a namespace, the
// cluster), lives across storms and lists many members, so it is never
// a scenario run's own task.
func (t *Transcript) FirstIncident() (Incident, bool) {
	// The first JSON value: core-agent's inbox bundle goes on after it.
	var in Incident
	if err := json.NewDecoder(strings.NewReader(t.FirstMessage())).Decode(&in); err != nil || in.Name == "" || in.Kind == "" || in.Kind == "storm" {
		return Incident{}, false
	}
	return in, true
}

// About reports whether the incident is about the scenario workload
// workload in namespace ns: the object it names is the workload or one
// of its pods ("<workload>-<replicaset hash>-<pod hash>").
func (in Incident) About(ns, workload string) bool {
	return in.Namespace == ns && (in.Name == workload || strings.HasPrefix(in.Name, workload+"-"))
}
