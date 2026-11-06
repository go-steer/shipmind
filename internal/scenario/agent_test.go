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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// stream is an event stream shaped like mast's: the incident from
// lookout, the coordinator delegating, a specialist's tool call and its
// result (role "user", from the specialist), a function response from
// "user" with no text, the coordinator's answer, then a repeat of the
// incident and a one-line acknowledgement.
const stream = `event: capabilities
data: {"protocol_version":"1.7.0"}

event: usage-update
data: {"tokens_in_total":10,"cost_usd_total":0.042,"turns_total":3}

event: agent
data: {"seq":3,"event":{"author":"user","content":{"role":"user","parts":[{"text":"{\"kind\":\"k8s-event\",\"name\":\"missing-env-1021-120000-abc\"}"}]}}}

data: {"seq":4,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"functionCall":{"name":"_fallback"}}]}}}

data: {"seq":7,"event":{"author":"_fallback","content":{"role":"model","parts":[{"text":"looking at the pod"}]}}}

data: {"seq":9,"event":{"author":"_fallback","content":{"role":"user","parts":[{"text":"tool result text"}]}}}

data: {"seq":23,"event":{"author":"user","content":{"role":"user","parts":[{"functionResponse":{}}]}}}

data: {"seq":29,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"text":"Cause: DATABASE_URL is missing from ConfigMap missing-env-1021-120000-config."}]}}}

data: {"seq":30,"event":{"author":"user","content":{"role":"user","parts":[{"text":"{\"kind\":\"k8s-event\",\"repeat\":true}"}]}}}

data: {"seq":31,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"text":"Still failing: same incident."}]}}}

data: not json
`

func TestParseEventsAndAnswer(t *testing.T) {
	tr, err := ParseEvents(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Events) != 8 || tr.Cost != 0.042 {
		t.Fatalf("%d events, cost %v", len(tr.Events), tr.Cost)
	}
	answer, superseded := tr.Answer()
	if answer != "Cause: DATABASE_URL is missing from ConfigMap missing-env-1021-120000-config." || !superseded {
		t.Errorf("Answer = %q, superseded=%v", answer, superseded)
	}
	if !tr.Mentions("missing-env-1021-120000-abc") || tr.Mentions("other-workload") {
		t.Error("Mentions wrong")
	}
}

func TestAnswerEdgeCases(t *testing.T) {
	parse := func(s string) *Transcript {
		t.Helper()
		tr, err := ParseEvents(strings.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	// No incoming message yet.
	if a, _ := parse(`data: {"seq":1,"event":{"author":"x","content":{"role":"model","parts":[{"text":"hi"}]}}}`).Answer(); a != "" {
		t.Errorf("answer without a message = %q", a)
	}
	// Still working: no answer, not superseded.
	if a, s := parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}`).Answer(); a != "" || s {
		t.Errorf("answer before any reply = %q, %v", a, s)
	}
	// No coordinator: the last non-user text counts.
	tr := parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}
data: {"seq":2,"event":{"author":"agent","content":{"role":"model","parts":[{"text":"first"},{"text":"second"}]}}}`)
	if a, s := tr.Answer(); a != "first\nsecond" || s {
		t.Errorf("answer = %q, %v", a, s)
	}
	// Another message before any answer joins the turn (lookout's update
	// while the agent investigates); the specialist's text doesn't count
	// when a coordinator answers later.
	tr = parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}
data: {"seq":2,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"functionCall":{"name":"_fallback"}}]}}}
data: {"seq":3,"event":{"author":"_fallback","content":{"role":"model","parts":[{"text":"looking"}]}}}
data: {"seq":4,"event":{"author":"user","content":{"role":"user","parts":[{"text":"update"}]}}}
data: {"seq":5,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"text":"diagnosis"}]}}}`)
	if a, s := tr.Answer(); a != "diagnosis" || s {
		t.Errorf("answer with a mid-turn message = %q, %v", a, s)
	}
	// Without a coordinator, any text is an answer, so the next message
	// supersedes it.
	tr = parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}
data: {"seq":2,"event":{"author":"agent","content":{"role":"model","parts":[{"text":"diagnosis"}]}}}
data: {"seq":3,"event":{"author":"user","content":{"role":"user","parts":[{"text":"repeat"}]}}}
data: {"seq":4,"event":{"author":"agent","content":{"role":"model","parts":[{"text":"ack"}]}}}`)
	if a, s := tr.Answer(); a != "diagnosis" || !s {
		t.Errorf("answer without a coordinator = %q, %v", a, s)
	}
}

func TestRouter(t *testing.T) {
	var gotActor, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor, gotAuth = r.Header.Get("ate-target-actor"), r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/sessions":
			fmt.Fprint(w, `{"sessions":[{"sessionID":"s1","status":"active","app":"mast"}]}`)
		case "/sessions/s1/status":
			fmt.Fprint(w, `{"state":"idle"}`)
		case "/sessions/s1/events":
			fmt.Fprint(w, stream)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stays open, as mast's does
		default:
			http.Error(w, "no such session", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r := &Router{Base: srv.URL + "/", Atespace: "agents", Token: "tok", EventsWait: 200 * time.Millisecond}
	ctx := context.Background()

	sessions, err := r.Sessions(ctx, "lookout-1")
	if err != nil || len(sessions) != 1 || sessions[0].ID != "s1" || sessions[0].Status != "active" {
		t.Fatalf("Sessions = %+v, %v", sessions, err)
	}
	if gotActor != "agents/lookout-1" || gotAuth != "Bearer tok" {
		t.Errorf("headers %q, %q", gotActor, gotAuth)
	}
	if state, err := r.State(ctx, "lookout-1", "s1"); err != nil || state != "idle" {
		t.Errorf("State = %q, %v", state, err)
	}
	tr, err := r.Events(ctx, "lookout-1", "s1")
	if err != nil || len(tr.Events) != 8 {
		t.Fatalf("Events = %v, %v", tr, err)
	}
	if _, err := r.State(ctx, "lookout-1", "nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing session: err = %v", err)
	}
	if _, err := r.Events(ctx, "lookout-1", "nope"); err == nil {
		t.Error("Events on a missing session succeeded")
	}
	if _, err := (&Router{Base: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}}).Sessions(ctx, "x"); err == nil {
		t.Error("unreachable router succeeded")
	}
	if _, err := (&Router{Base: "::bad"}).Sessions(ctx, "x"); err == nil {
		t.Error("bad URL accepted")
	}
}

// A real core-agent v3.1.0-dev.3 stream (testdata/core-agent-incident.sse,
// lookout's inject of an incident as captured from /events): the message
// arrives wrapped in core-agent's inbox bundle, and the incident is still
// found in it.
func TestCoreAgentIncident(t *testing.T) {
	f, err := os.Open("testdata/core-agent-incident.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr, err := ParseEvents(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tr.Events[0].Texts[0], inboxHeader) {
		t.Fatalf("fixture's first event isn't an inbox bundle: %q", tr.Events[0].Texts[0])
	}
	in, ok := tr.FirstIncident()
	if !ok || !in.About("triage-lab", "missing-env-1103-120000") {
		t.Errorf("FirstIncident = %+v, %v", in, ok)
	}
}

func TestMessageText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"kind":"pod.crashloop"}`, `{"kind":"pod.crashloop"}`},                                         // mast: as sent
		{"[Inbox]\n- from anon: {\"kind\":\"x\"}\n\nHow to handle", "{\"kind\":\"x\"}\n\nHow to handle"}, // core-agent
		{"[Inbox]\n- from sa:lookout: hello\n- from bob: hi\n", "hello\n- from bob: hi\n"},               // first bullet
		{"[Inbox]\n- {\"kind\":\"x\",\"a\":\"b: c\"}\n", "{\"kind\":\"x\",\"a\":\"b: c\"}\n"},            // no sender
		{"[Inbox]\n- from a b: c", "from a b: c"},                                                        // not a sender
	} {
		if got := messageText(tc.in); got != tc.want {
			t.Errorf("messageText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A subagent started in the background reports in a later "[Background
// reports]" turn; that is not a new message, and the answer is what the
// agent says after it.
func TestAnswerAfterBackgroundReport(t *testing.T) {
	tr := &Transcript{Events: []Event{
		{Seq: 1, Author: "user", Role: "user", Texts: []string{"[Inbox]\n- from anon: {\"kind\":\"pod.crashloop\"}"}},
		{Seq: 2, Author: "core_agent", Role: "model", Texts: []string{"Delegated to _fallback; waiting for its finding."}},
		{Seq: 3, Author: "user", Role: "user", Texts: []string{"[Background reports]\n- _fallback (completed): DATABASE_URL is missing"}},
		{Seq: 4, Author: "core_agent", Role: "model", Texts: []string{"The pod crashes because DATABASE_URL is unset in demo-config."}},
	}}
	answer, superseded := tr.Answer()
	if superseded || !strings.Contains(answer, "DATABASE_URL is unset") {
		t.Errorf("Answer = %q, superseded %v", answer, superseded)
	}
}

// core-agent's /status says idle while a background subagent still
// works; State reports that as working, and mast's missing /agents
// route (404) as nothing running.
func TestStateWithSubagents(t *testing.T) {
	agents := `{"agents":[{"id":"a1","name":"_fallback","status":"running"}]}`
	agentsCode := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sessions/s1/status":
			fmt.Fprint(w, `{"state":"idle"}`)
		case "/sessions/s1/agents":
			w.WriteHeader(agentsCode)
			fmt.Fprint(w, agents)
		default:
			http.Error(w, "no such session", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r := &Router{Base: srv.URL, Atespace: "agents", Token: "tok"}
	ctx := context.Background()
	if s, err := r.State(ctx, "t", "s1"); err != nil || s != "working" {
		t.Errorf("running subagent: State = %q, %v", s, err)
	}
	agents = `{"agents":[{"id":"a1","name":"_fallback","status":"completed"}]}`
	if s, err := r.State(ctx, "t", "s1"); err != nil || s != "idle" {
		t.Errorf("finished subagent: State = %q, %v", s, err)
	}
	agentsCode, agents = http.StatusInternalServerError, "boom"
	if _, err := r.State(ctx, "t", "s1"); err == nil {
		t.Error("a failing /agents read reported a state")
	}
	agentsCode, agents = http.StatusNotFound, "not found"
	if s, err := r.State(ctx, "t", "s1"); err != nil || s != "idle" {
		t.Errorf("no /agents route (mast): State = %q, %v", s, err)
	}
}

// GET /usage as core-agent v3.1.0-rc.1 answered it for a missing-env
// incident (testdata/core-agent-usage.json, captured from a kept task).
func TestRouterUsage(t *testing.T) {
	body, err := os.ReadFile("testdata/core-agent-usage.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/s1/usage" {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	r := &Router{Base: srv.URL, Atespace: "agents", Token: "tok"}
	u, err := r.Usage(context.Background(), "t", "s1")
	if err != nil {
		t.Fatal(err)
	}
	// core-agent reports thinking beside output: kept as is.
	n := u.Normalize("core-agent/dfaff57e", time.Date(2026, 11, 4, 16, 31, 0, 0, time.UTC))
	if n.Agent != "core-agent/dfaff57e" || n.ReadAt != "2026-11-04T16:31:00Z" || n.ModelCalls != 8 || n.InputTokens != 113873 || n.CachedInputTokens != 75522 ||
		n.OutputTokens != 2160 || n.ThinkingTokens != 2160 || n.CostUSD < 0.05 || n.CostUSD > 0.051 {
		t.Errorf("totals %+v", n)
	}
	// Call 2: 14766 in, 210 out, 538 thinking (15514 total); call 3 the
	// first from the cache.
	c := n.Calls
	if len(c) != 8 || c[0].Call != 1 || c[0].Model != "gemini-3.8-flash" || c[1].At != "2026-11-04T16:30:18.284741121Z" ||
		c[1].InputTokens != 14766 || c[1].OutputTokens != 210 || c[1].ThinkingTokens != 538 || c[2].CachedInputTokens != 12587 {
		t.Errorf("calls %+v", c)
	}
	if _, err := r.Usage(context.Background(), "t", "nope"); err == nil {
		t.Error("Usage on a missing session succeeded")
	}
}

// mast reports thinking inside its output count (its UsageTotals doc:
// "ThoughtsTokens is the thinking subset of it, not an addition"), so
// Normalize takes it out there, and only there.
func TestUsageNormalizeMast(t *testing.T) {
	var u AgentUsage
	if err := json.Unmarshal([]byte(`{"overall":{"input_tokens":5000,"input_tokens_cached":1000,"output_tokens":900,"thoughts_tokens":600,"turns":2,"cost_usd":0.01},
		"per_turn":[{"turn":41,"model":"gemini-3.8-flash","input_tokens":2000,"output_tokens":300,"thoughts_tokens":200},{"turn":42,"input_tokens":3000,"output_tokens":600,"thoughts_tokens":400}]}`), &u); err != nil {
		t.Fatal(err)
	}
	n := u.Normalize("mast/922e549", time.Unix(0, 0))
	if n.OutputTokens != 300 || n.ThinkingTokens != 600 || n.Calls[0].OutputTokens != 100 || n.Calls[1].OutputTokens != 200 || n.Calls[0].Call != 41 {
		t.Errorf("mast normalised: %+v", n)
	}
	if c := u.Normalize("core-agent/x", time.Unix(0, 0)); c.OutputTokens != 900 {
		t.Errorf("core-agent output %d, want it as reported", c.OutputTokens)
	}
}

// The agent names itself in the stream's capabilities frame.
func TestTranscriptServer(t *testing.T) {
	f, err := os.Open("testdata/core-agent-incident.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr, err := ParseEvents(f)
	if err != nil || !strings.HasPrefix(tr.Server, "core-agent/") {
		t.Errorf("Server = %q, %v", tr.Server, err)
	}
}

// Replay keeps the stream as read, for the archive, and says when it ran
// past its limit; a frame that doesn't parse is still kept.
func TestRouterReplay(t *testing.T) {
	body := "event: capabilities\ndata: {\"server\":\"core-agent/v3.1.0\"}\n\nevent: agent\ndata: {\"seq\":1,\"event\":{\"author\":\"user\"}}\n\ndata: not json\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/s1/events" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	r := &Router{Base: srv.URL, Atespace: "agents", Token: "tok", EventsWait: 200 * time.Millisecond}
	rp, err := r.Replay(context.Background(), "t", "s1")
	if err != nil || string(rp.Raw) != body || rp.Server != "core-agent/v3.1.0" || rp.Truncated {
		t.Fatalf("Replay = %+v, %v", rp, err)
	}
	r.MaxRaw = 40
	if rp, err := r.Replay(context.Background(), "t", "s1"); err != nil || len(rp.Raw) != 40 || !rp.Truncated {
		t.Errorf("limited Replay = %d bytes, truncated %v, %v", len(rp.Raw), rp.Truncated, err)
	}
	if _, err := r.Replay(context.Background(), "t", "nope"); err == nil {
		t.Error("Replay of a missing session succeeded")
	}
}

// Tool calls come out of the stream in order, from functionCall parts,
// as core-agent's attach frames carry them.
func TestParseEventsCalls(t *testing.T) {
	stream := `event: agent
data: {"seq":2,"event":{"author":"core_agent","content":{"role":"model","parts":[{"functionCall":{"name":"gke_get_k8s_resource","args":{"name":"p"}}},{"functionCall":{"name":"gke_get_k8s_logs","args":{}}}]}}}

event: agent
data: {"seq":5,"event":{"author":"core_agent","content":{"role":"user","parts":[{"functionResponse":{"name":"gke_get_k8s_resource","response":{}}}]}}}

event: agent
data: {"seq":6,"event":{"author":"core_agent","content":{"role":"model","parts":[{"text":"thinking"},{"functionCall":{"name":"gke_get_k8s_resource"}}]}}}
`
	tr, err := ParseEvents(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tr.Calls(), []string{"gke_get_k8s_resource", "gke_get_k8s_logs", "gke_get_k8s_resource"}; !slices.Equal(got, want) {
		t.Errorf("Calls() = %v, want %v", got, want)
	}
}
