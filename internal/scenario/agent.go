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
	// RetryDelay is the base pause between retries when the router returns
	// 503 Service Unavailable (Substrate v1.0.1 returns 503 while an actor
	// is still resuming). Defaults to 1s.
	RetryDelay time.Duration
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

// maxResumeAttempts bounds how many times Router tries a request that the
// router answers with 503 Service Unavailable (slow actor resume in
// Substrate v1.0.1; each retry joins the in-flight resume).
const (
	maxResumeAttempts = 5
	defaultRetryDelay = time.Second
)

func (r *Router) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: routerTimeout}
}

func (r *Router) do(ctx context.Context, method, task, path string, body []byte) (*http.Response, error) {
	target := strings.TrimSuffix(r.Base, "/") + path
	delay := r.RetryDelay
	if delay <= 0 {
		delay = defaultRetryDelay
	}
	client := r.client()
	for attempt := 0; ; attempt++ {
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reqBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("ate-target-actor", r.Atespace+"/"+task)
		req.Header.Set("Authorization", "Bearer "+r.Token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusServiceUnavailable || attempt+1 >= maxResumeAttempts {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		timer := time.NewTimer(delay << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Router) get(ctx context.Context, task, path string) (*http.Response, error) {
	resp, err := r.do(ctx, http.MethodGet, task, path, nil)
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

// postJSON sends body to the task as JSON and decodes a 2xx answer into
// v (nil: discard it).
func (r *Router) postJSON(ctx context.Context, task, path string, body, v any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := r.do(ctx, http.MethodPost, task, path, b)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{code: resp.StatusCode, msg: fmt.Sprintf("POST %s on %s: %s: %s", path, task, resp.Status, strings.TrimSpace(string(msg)))}
	}
	if v == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// CreateSession opens a session on the task (core-agent's POST
// /sessions), owned by the attach token's caller, and returns its ID.
func (r *Router) CreateSession(ctx context.Context, task string) (string, error) {
	var out struct {
		ID string `json:"sessionID"`
	}
	if err := r.postJSON(ctx, task, "/sessions", struct{}{}, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("POST /sessions on %s: the answer names no session", task)
	}
	return out.ID, nil
}

// Inject sends the session a message, as a person in a thread would.
func (r *Router) Inject(ctx context.Context, task, session, message string) error {
	return r.postJSON(ctx, task, "/sessions/"+url.PathEscape(session)+"/inject", map[string]string{"message": message}, nil)
}

// SubagentEvents reads the turns a subagent (a specialist) has taken
// under the session, which core-agent records apart from the session's
// own stream. known is false when the agent has no subagent of that name
// (core-agent's 404); a subagent it has but that has not run comes back
// known, with no events. The transcript's Truncated says the history ran
// past the one page read.
func (r *Router) SubagentEvents(ctx context.Context, task, session, name string) (t *Transcript, known bool, err error) {
	var out struct {
		Events []struct {
			Seq   int       `json:"seq"`
			Event wireEvent `json:"event"`
		} `json:"events"`
		Truncated bool `json:"truncated"`
	}
	// The largest page core-agent serves, from the start of the
	// specialist's history: all of it in any run a scenario bounds. A
	// longer one is cut at its end, and says so.
	err = r.getJSON(ctx, task, "/sessions/"+url.PathEscape(session)+"/agents/"+url.PathEscape(name)+"/events?limit=5000", &out)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusNotFound:
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	t = &Transcript{Truncated: out.Truncated}
	for _, f := range out.Events {
		t.Events = append(t.Events, f.Event.event(f.Seq))
	}
	return t, true, nil
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
	// Truncated: a subagent's history ran past what was read.
	Truncated bool
}

// Event is one agent event: who wrote it and its text parts.
type Event struct {
	Seq    int
	Author string
	Role   string
	Texts  []string
	// Calls are the tools the event calls, as the agent names them.
	Calls []string
	// Failed are the tool results in the event that are errors, as
	// "<tool>: <message>": core-agent answers a failed call (an MCP
	// result with isError, a transport failure, a denied permission) with
	// a response carrying an "error".
	Failed []string
	// Results are the tool results in the event that are not errors.
	Results []ToolResult
}

// ToolResult is one successful tool result: the tool, as the agent names
// it, and what it returned (see resultOutput).
type ToolResult struct {
	Tool   string `json:"tool"`
	Output string `json:"output"`
	// Method is how core-agent digested the result before the model saw
	// it: "passthrough" (whole), "structural_json" (long arrays and
	// strings cut, fields like status kept) or "llm_fallback" (a model's
	// summary). Empty for a result that was not digested.
	Method string `json:"method,omitempty"`
}

// MethodSummarized is the digest method whose output is a model's prose,
// which a pattern written for the tool's own output can't be held to.
const MethodSummarized = "llm_fallback"

// wireEvent is an agent event as mast and core-agent serialize it, in
// the session's stream and in a subagent's history alike.
type wireEvent struct {
	Author  string `json:"author"`
	Content *struct {
		Role  string `json:"role"`
		Parts []struct {
			Text         string `json:"text"`
			FunctionCall *struct {
				Name string `json:"name"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				Name     string          `json:"name"`
				Response json.RawMessage `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"content"`
}

// maxFailure bounds how much of a tool's error message a result keeps.
const maxFailure = 300

func (w wireEvent) event(seq int) Event {
	e := Event{Seq: seq, Author: w.Author}
	c := w.Content
	if c == nil {
		return e
	}
	e.Role = c.Role
	for _, p := range c.Parts {
		if p.Text != "" {
			e.Texts = append(e.Texts, p.Text)
		}
		if p.FunctionCall != nil && p.FunctionCall.Name != "" {
			e.Calls = append(e.Calls, p.FunctionCall.Name)
		}
		if r := p.FunctionResponse; r != nil {
			// genai's response is an object; one that isn't is kept
			// whole as the result rather than costing the event.
			var response map[string]json.RawMessage
			if json.Unmarshal(r.Response, &response) != nil {
				e.Results = append(e.Results, ToolResult{Tool: r.Name, Output: string(r.Response)})
				continue
			}
			if raw, ok := response["error"]; ok && string(raw) != "null" && string(raw) != `""` {
				msg := string(raw)
				var s string
				if json.Unmarshal(raw, &s) == nil {
					msg = s
				}
				if len(msg) > maxFailure {
					msg = msg[:maxFailure] + "…"
				}
				e.Failed = append(e.Failed, r.Name+": "+msg)
				continue
			}
			output, method := resultOutput(response)
			e.Results = append(e.Results, ToolResult{Tool: r.Name, Output: output, Method: method})
		}
	}
	return e
}

// resultOutput is a successful tool response as text, and how core-agent
// digested it.
//
// An MCP tool's response is {"output": ...}: its text, or its structured
// content. core-agent wraps that, by default, as {"digest": "<JSON
// text>", "method": ...}: the same object as text when it is small, and
// pruned when it is not, in which case a tool's JSON text has been parsed
// into the object in its place. Both layers are unwrapped, so a pattern
// written against the tool's own JSON matches either. Any other tool's
// response is kept whole, as JSON.
func resultOutput(response map[string]json.RawMessage) (output, method string) {
	if raw, ok := response["digest"]; ok {
		var digest string
		if json.Unmarshal(raw, &digest) == nil {
			_ = json.Unmarshal(response["method"], &method)
			var inner map[string]json.RawMessage
			if json.Unmarshal([]byte(digest), &inner) != nil {
				return digest, method
			}
			if _, ok := inner["output"]; !ok {
				return digest, method
			}
			response = inner
		}
	}
	if raw, ok := response["output"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, method
		}
		return string(raw), method
	}
	b, err := json.Marshal(response)
	if err != nil {
		return "", method
	}
	return string(b), method
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
			Seq    *int      `json:"seq"`
			Event  wireEvent `json:"event"`
			Cost   *float64  `json:"cost_usd_total"`
			Server string    `json:"server"`
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
		t.Events = append(t.Events, d.Event.event(*d.Seq))
	}
	return t, sc.Err()
}

// Calls returns every tool the session called, in order.
func (t *Transcript) Calls() []string {
	var calls []string
	for _, e := range t.Events {
		calls = append(calls, e.Calls...)
	}
	return calls
}

// Failures returns every failed tool result in the transcript, in order.
func (t *Transcript) Failures() []string {
	var failed []string
	for _, e := range t.Events {
		failed = append(failed, e.Failed...)
	}
	return failed
}

// Results returns every successful tool result in the transcript, in
// order.
func (t *Transcript) Results() []ToolResult {
	var results []ToolResult
	for _, e := range t.Events {
		results = append(results, e.Results...)
	}
	return results
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
	answer, superseded, _ = t.answerSpan()
	return answer, superseded
}

// AnswerCalls returns the tools the session called for its answer: up to
// a later message that supersedes it (lookout re-sending the incident),
// not the calls made for that.
func (t *Transcript) AnswerCalls() []string {
	_, _, end := t.answerSpan()
	var calls []string
	for _, e := range t.Events[:end] {
		calls = append(calls, e.Calls...)
	}
	return calls
}

// answerSpan is Answer, plus the index of the event that ended it: the
// superseding message, or the end of the transcript.
func (t *Transcript) answerSpan() (answer string, superseded bool, end int) {
	end = len(t.Events)
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
		return "", false, end
	}
	var coordinator, other string
	for i, e := range t.Events[first+1:] {
		if isUser(e) {
			if coordinator != "" || (!hasCoordinator && other != "") {
				superseded, end = true, first+1+i
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
		return coordinator, superseded, end
	}
	return other, superseded, end
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
