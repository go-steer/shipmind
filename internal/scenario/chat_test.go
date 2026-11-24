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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// leewayOK and leewayUnreached are leeway reports as its report tool
// returns them: compact JSON, a cluster it could not list carrying status
// "error" inside a result that is not itself an error.
const (
	leewayOK        = `{"schema_version":"leeway.report/v1alpha1","clusters":[{"cluster":"example-project/us-central1/demo","status":"ok"}],"summary":{"clusters_total":1,"clusters_succeeded":1,"clusters_failed":0}}`
	leewayUnreached = `{"schema_version":"leeway.report/v1alpha1","clusters":[{"cluster":"example-project/us-central1/demo","status":"error","error":"list nodes: Unauthorized"}],"summary":{"clusters_total":1,"clusters_succeeded":0,"clusters_failed":1}}`
)

const (
	chatClusterPath = "projects/example-project/locations/us-central1/clusters/demo"
	chatTask        = "apiVersion: ax.io/v1alpha1\nkind: Task\nmetadata:\n  name: canary-1\n  atespace: agents\nspec: {}\n"
	chatWorkspace   = "apiVersion: ax.io/v1alpha1\nkind: Workspace\nmetadata:\n  name: canary-1\n  atespace: agents\n"
)

// chatWorld is an ax and an agent on a virtual clock: the task accepts a
// session at readyAt and has answered by answerAt after the question.
type chatWorld struct {
	now, asked        time.Time
	readyAt, answerAt time.Duration
	answer            string
	mainCalls         []string
	mainFailed        []string
	mainResults       []ToolResult
	// specialists are the turns of the subagents that ran, by name; one
	// the agent has but that did not run has none. unknown are names the
	// agent has no subagent for.
	specialists map[string]*Transcript
	unknown     []string

	failApply, failDelete string // a manifest kind
	failInject, failUsage bool
	failSubagent          bool
	neverIdle             bool

	ops      []string
	question string
}

func (w *chatWorld) Apply(_ context.Context, m []byte) error {
	kind := "Task"
	if strings.Contains(string(m), "kind: Workspace") {
		kind = "Workspace"
	}
	if w.failApply == kind {
		return errors.New("ax apply: refused")
	}
	w.ops = append(w.ops, "apply "+kind)
	return nil
}

func (w *chatWorld) DeleteTask(_ context.Context, name string) error {
	if w.failDelete == "Task" {
		return errors.New("ax delete: refused")
	}
	w.ops = append(w.ops, "delete task "+name)
	return nil
}

func (w *chatWorld) DeleteWorkspace(_ context.Context, name string) error {
	if w.failDelete == "Workspace" {
		return errors.New("ax delete: refused")
	}
	w.ops = append(w.ops, "delete workspace "+name)
	return nil
}

func (w *chatWorld) CreateSession(context.Context, string) (string, error) {
	if w.readyAt < 0 || w.now.Sub(t0) < w.readyAt {
		return "", errors.New("502: the actor is starting")
	}
	return "s-1", nil
}

func (w *chatWorld) Inject(_ context.Context, _, _, message string) error {
	if w.failInject {
		return errors.New("inject: 503")
	}
	w.question, w.asked = message, w.now
	return nil
}

func (w *chatWorld) answered() bool {
	return w.answerAt >= 0 && !w.asked.IsZero() && w.now.Sub(w.asked) >= w.answerAt
}

func (w *chatWorld) Sessions(context.Context, string) ([]Session, error) {
	return []Session{{ID: "s-1", Status: "active"}}, nil
}

func (w *chatWorld) State(context.Context, string, string) (string, error) {
	if w.answered() && !w.neverIdle {
		return "idle", nil
	}
	return "running", nil
}

func (w *chatWorld) Usage(context.Context, string, string) (*AgentUsage, error) {
	if w.failUsage {
		return nil, errors.New("usage: 503")
	}
	var u AgentUsage
	err := json.Unmarshal([]byte(`{"overall":{"input_tokens":900,"output_tokens":40,"turns":3,"cost_usd":0.04}}`), &u)
	return &u, err
}

func (w *chatWorld) Events(context.Context, string, string) (*Transcript, error) {
	tr := &Transcript{Server: "core-agent/test", Cost: 0.04, Events: []Event{{Seq: 1, Author: "user", Texts: []string{w.question}}}}
	if w.answered() {
		tr.Events = append(tr.Events,
			Event{Seq: 2, Author: "gke-chat_coordinator", Role: "model", Calls: w.mainCalls, Failed: w.mainFailed, Results: w.mainResults},
			Event{Seq: 9, Author: "gke-chat_coordinator", Role: "model", Texts: []string{w.answer}})
	}
	return tr, nil
}

func (w *chatWorld) SubagentEvents(_ context.Context, _, _, name string) (*Transcript, bool, error) {
	if w.failSubagent {
		return nil, false, errors.New("agents: 500")
	}
	if slices.Contains(w.unknown, name) {
		return nil, false, nil
	}
	if t, ok := w.specialists[name]; ok {
		return t, true, nil
	}
	return &Transcript{}, true, nil
}

func (w *chatWorld) Replay(context.Context, string, string) (*Replay, error) {
	return &Replay{Raw: []byte("data: {}\n"), Server: "core-agent/test"}, nil
}

func newChatWorld(t *testing.T) (*chatWorld, *Scenario, ChatRun, Options) {
	t.Helper()
	s, err := Load("../../e2e/scenarios/posture-audit")
	if err != nil {
		t.Fatal(err)
	}
	w := &chatWorld{
		now: t0, readyAt: time.Minute, answerAt: 3 * time.Minute,
		answer:      "Cluster demo: the audit found 2 warnings; leeway found no placement skew.",
		mainCalls:   []string{"posture-audit"},
		mainResults: []ToolResult{{Tool: "posture-audit", Output: `{"title":"posture"}`}},
		specialists: map[string]*Transcript{"posture-audit": {Events: []Event{
			{Seq: 1, Author: "posture-audit", Role: "model", Calls: []string{"lookout_k8s_audit_all", "leeway_report"}},
			{Seq: 2, Author: "posture-audit", Role: "user", Results: []ToolResult{
				{Tool: "lookout_k8s_audit_all", Output: "scanned=40 findings=2"},
				{Tool: "leeway_report", Output: leewayOK},
			}},
		}}},
	}
	o := Options{
		Poll:  30 * time.Second,
		Now:   func() time.Time { return w.now },
		Sleep: func(_ context.Context, d time.Duration) error { w.now = w.now.Add(d); return nil },
	}
	return w, s, ChatRun{Task: []byte(chatTask), Workspace: []byte(chatWorkspace), Cluster: chatClusterPath}, o
}

func TestRunChatPasses(t *testing.T) {
	w, s, run, o := newChatWorld(t)
	var logs []string
	o.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	grader := &fakeGrader{}
	o.Judge = grader
	arch := &fakeArchive{}
	o.Archive = arch
	r, err := RunChat(context.Background(), s, w, w, run, o)
	if err != nil {
		t.Fatalf("RunChat: %v", err)
	}
	if !r.Pass || !r.Correct || r.Task != "canary-1" || r.Session != "s-1" || r.Cluster != chatClusterPath || r.CostUSD != 0.04 {
		t.Fatalf("result %+v", r)
	}
	// The Workspace before the task that mounts it; the task deleted
	// before the Workspace.
	if want := []string{"apply Workspace", "apply Task", "delete task canary-1", "delete workspace canary-1"}; !slices.Equal(w.ops, want) {
		t.Errorf("ops %v, want %v", w.ops, want)
	}
	if !strings.Contains(w.question, chatClusterPath) {
		t.Errorf("the question doesn't name the cluster: %q", w.question)
	}
	// The specialist's calls count, after the main agent's.
	tj := r.Trajectory
	if tj == nil || !slices.Equal(tj.ToolCalls, []string{"posture-audit", "lookout_k8s_audit_all", "leeway_report"}) || len(tj.Failures) != 0 || len(tj.ToolErrors) != 0 || tj.ModelCalls != 3 {
		t.Errorf("trajectory %+v", tj)
	}
	if r.IncidentAfter != "1m0s" || r.AnswerAfter == "" || r.Usage == nil || r.Usage.Agent != "core-agent/test" {
		t.Errorf("timings and usage: %+v", r)
	}
	if r.Judge == nil || !strings.Contains(grader.expected, chatClusterPath) {
		t.Errorf("judge %+v, graded against %q", r.Judge, grader.expected)
	}
	if arch.dir == "" || arch.files["transcript.sse"] == nil || arch.files["usage.json"] == nil ||
		!strings.Contains(string(arch.files["meta.json"]), `"kind": "chat"`) || !strings.Contains(string(arch.files["result.json"]), `"pass": true`) {
		t.Errorf("archive %q: %d files", arch.dir, len(arch.files))
	}
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.HasPrefix(l, "trajectory: 3 tool calls") }) {
		t.Errorf("logs %v", logs)
	}
}

func TestRunChatFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*chatWorld)
		want   string
	}{
		{"never ready", func(w *chatWorld) { w.readyAt = -1 }, "accepted no session within 5m0s: 502"},
		{"question not sent", func(w *chatWorld) { w.failInject = true }, "sending the question: inject: 503"},
		{"no answer", func(w *chatWorld) { w.answerAt = -1 }, "no settled answer within"},
		{"unsettled answer", func(w *chatWorld) { w.neverIdle = true }, "no settled answer within"},
		{"another cluster", func(w *chatWorld) { w.answer = "Cluster prod looks healthy." }, "the answer lacks demo"},
		{"says a tool failed", func(w *chatWorld) { w.answer = "TOOL FAILURE: leeway_report\nFor demo, the audit found 2 warnings." }, "TOOL FAILURE)"},
		// The specialist never ran, so neither tool was called.
		{"no delegation", func(w *chatWorld) { w.specialists = nil }, "trajectory: never called k8s_audit_all"},
		{"one tool skipped", func(w *chatWorld) {
			w.specialists["posture-audit"].Events[0].Calls = []string{"lookout_k8s_audit_all"}
		}, "trajectory: never called report"},
		// The call failed and never succeeded, and the model wrote a
		// plausible answer anyway: the error result fails the run.
		{"tool error", func(w *chatWorld) {
			w.specialists["posture-audit"].Events[1] = Event{Seq: 2, Author: "posture-audit", Role: "user",
				Results: []ToolResult{{Tool: "lookout_k8s_audit_all", Output: "scanned=40"}},
				Failed:  []string{"leeway_report: Tool execution failed. Details: leeway report: no clusters resolved"}}
		}, "trajectory: a tool call failed: leeway_report: Tool execution failed. Details: leeway report: no clusters resolved"},
		// The call succeeded, and what it returned says it reached
		// nothing. This is how leeway reports a cluster it could not
		// authenticate to.
		{"leeway reached nothing", func(w *chatWorld) {
			w.specialists["posture-audit"].Events[1].Results[1].Output = leewayUnreached
		}, `trajectory: leeway_report returned a result matching "status":\s*"error"`},
		{"no leeway result", func(w *chatWorld) {
			w.specialists["posture-audit"].Events[1].Results = w.specialists["posture-audit"].Events[1].Results[:1]
		}, "trajectory: report returned no result to check"},
		{"main agent tool error", func(w *chatWorld) {
			w.mainFailed, w.mainResults = []string{"posture-audit: permission denied"}, nil
		}, "a tool call failed: posture-audit: permission denied"},
		{"workspace not applied", func(w *chatWorld) { w.failApply = "Workspace" }, "applying workspace canary-1: ax apply: refused"},
		{"task not deleted", func(w *chatWorld) { w.failDelete = "Task" }, "deleting task canary-1: ax delete: refused"},
		{"workspace not deleted", func(w *chatWorld) { w.failDelete = "Workspace" }, "deleting workspace canary-1: ax delete: refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s, run, o := newChatWorld(t)
			tc.mutate(w)
			r, err := RunChat(context.Background(), s, w, w, run, o)
			if err != nil {
				t.Fatalf("RunChat: %v", err)
			}
			if r.Pass || !slices.ContainsFunc(r.Failures, func(f string) bool { return strings.Contains(f, tc.want) }) {
				t.Errorf("pass %v, failures %q, want one containing %q", r.Pass, r.Failures, tc.want)
			}
			// Whatever went wrong, nothing is left behind.
			if tc.name != "task not deleted" && tc.name != "workspace not applied" && !slices.Contains(w.ops, "delete task canary-1") {
				t.Errorf("the task was not deleted: %v", w.ops)
			}
			if tc.name != "workspace not deleted" && !slices.Contains(w.ops, "delete workspace canary-1") {
				t.Errorf("the workspace was not deleted: %v", w.ops)
			}
		})
	}
}

// A task that can't be applied fails the run. The Workspace applied for
// it is removed, and so is the task, in case the apply that failed
// created it anyway.
func TestRunChatTaskNotApplied(t *testing.T) {
	w, s, run, o := newChatWorld(t)
	w.failApply = "Task"
	r, err := RunChat(context.Background(), s, w, w, run, o)
	if err != nil || r.Pass || r.Task != "" || !strings.Contains(strings.Join(r.Failures, "\n"), "applying task canary-1: ax apply: refused") {
		t.Fatalf("result %+v, err %v", r, err)
	}
	if want := []string{"apply Workspace", "delete task canary-1", "delete workspace canary-1"}; !slices.Equal(w.ops, want) {
		t.Errorf("ops %v, want %v", w.ops, want)
	}
	// Deleting what a failed apply didn't create is not a second failure.
	w, s, run, o = newChatWorld(t)
	w.failApply, w.failDelete = "Task", "Task"
	var logs []string
	o.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	r, err = RunChat(context.Background(), s, w, w, run, o)
	if err != nil || len(r.Failures) != 1 || !slices.Contains(logs, "deleting task canary-1, whose apply failed: ax delete: refused") {
		t.Errorf("failures %q, logs %q, err %v", r.Failures, logs, err)
	}
}

// A tool that failed and then worked is a model's slip, not a canary's
// finding: noted, and the run passes.
func TestRunChatRetriedTool(t *testing.T) {
	w, s, run, o := newChatWorld(t)
	sp := w.specialists["posture-audit"]
	sp.Events = append([]Event{{Seq: 0, Author: "posture-audit", Role: "user", Failed: []string{"leeway_report: invalid arguments: unknown field"}}}, sp.Events...)
	r, err := RunChat(context.Background(), s, w, w, run, o)
	if err != nil || !r.Pass {
		t.Fatalf("result %+v, err %v", r, err)
	}
	if !slices.Contains(r.Trajectory.Notes, "a tool call failed and a later one succeeded: leeway_report: invalid arguments: unknown field") || len(r.Trajectory.ToolErrors) != 1 {
		t.Errorf("trajectory %+v", r.Trajectory)
	}
}

// A specialist the agent doesn't have, and one whose history was cut,
// are said so.
func TestRunChatSpecialistNotes(t *testing.T) {
	w, s, run, o := newChatWorld(t)
	w.unknown = []string{"posture-audit"}
	r, err := RunChat(context.Background(), s, w, w, run, o)
	if err != nil || r.Pass || !slices.Contains(r.Trajectory.Notes, "the agent has no specialist posture-audit") {
		t.Errorf("unknown: %+v, %v", r.Trajectory, err)
	}
	w, s, run, o = newChatWorld(t)
	w.specialists = nil
	r, _ = RunChat(context.Background(), s, w, w, run, o)
	if r.Pass || !slices.Contains(r.Trajectory.Notes, "specialist posture-audit did not run") {
		t.Errorf("did not run: %+v", r.Trajectory)
	}
	w, s, run, o = newChatWorld(t)
	w.specialists["posture-audit"].Truncated = true
	arch := &fakeArchive{}
	o.Archive = arch
	r, _ = RunChat(context.Background(), s, w, w, run, o)
	if !r.Pass || !slices.ContainsFunc(r.Trajectory.Notes, func(n string) bool { return strings.Contains(n, "its history ran past what was read") }) {
		t.Errorf("truncated: %+v", r.Trajectory)
	}
	// The specialist's turns, which decide the grade, are archived too.
	if !strings.Contains(string(arch.files["specialist-posture-audit.json"]), "leeway_report") {
		t.Errorf("archive lacks the specialist's turns: %d files", len(arch.files))
	}
}

func TestRunChatCannotStart(t *testing.T) {
	incident, err := Load("../../e2e/scenarios/missing-env")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*chatWorld, **Scenario, *ChatRun)
		want   string
	}{
		{"incident scenario", func(_ *chatWorld, s **Scenario, _ *ChatRun) { *s = incident }, "is not a chat scenario"},
		{"cluster name", func(_ *chatWorld, _ **Scenario, r *ChatRun) { r.Cluster = "demo" }, "want projects/<project>/locations/<location>/clusters/<name>"},
		{"unnamed task", func(_ *chatWorld, _ **Scenario, r *ChatRun) {
			r.Task = []byte("kind: Task\nmetadata:\n  atespace: agents\n")
		}, "the Task manifest: want kind Task with a metadata.name"},
		{"task is not yaml", func(_ *chatWorld, _ **Scenario, r *ChatRun) { r.Task = []byte(":\n\t- x") }, "the Task manifest:"},
		{"workspace of another kind", func(_ *chatWorld, _ **Scenario, r *ChatRun) { r.Workspace = []byte(chatTask) }, "the Workspace manifest: want kind Workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s, run, o := newChatWorld(t)
			tc.mutate(w, &s, &run)
			r, err := RunChat(context.Background(), s, w, w, run, o)
			if r != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("result %+v, err %v, want an error containing %q", r, err, tc.want)
			}
			if slices.Contains(w.ops, "apply Task") {
				t.Errorf("the task was applied: %v", w.ops)
			}
		})
	}
}

// -keep leaves the task and Workspace; a run with no Workspace applies
// and deletes only the task; a cancelled run still cleans up.
func TestRunChatKeepAndCancel(t *testing.T) {
	w, s, run, o := newChatWorld(t)
	o.Keep = true
	if r, err := RunChat(context.Background(), s, w, w, run, o); err != nil || !r.Pass {
		t.Fatalf("keep: %+v, %v", r, err)
	}
	if want := []string{"apply Workspace", "apply Task"}; !slices.Equal(w.ops, want) {
		t.Errorf("keep: ops %v, want %v", w.ops, want)
	}

	w, s, run, o = newChatWorld(t)
	run.Workspace = nil
	if r, err := RunChat(context.Background(), s, w, w, run, o); err != nil || !r.Pass {
		t.Fatalf("no workspace: %+v, %v", r, err)
	}
	if want := []string{"apply Task", "delete task canary-1"}; !slices.Equal(w.ops, want) {
		t.Errorf("no workspace: ops %v, want %v", w.ops, want)
	}

	w, s, run, o = newChatWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	o.Sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	arch := &fakeArchive{}
	o.Archive = arch
	r, err := RunChat(ctx, s, w, w, run, o)
	if !errors.Is(err, context.Canceled) || r == nil || r.Pass {
		t.Fatalf("cancelled: %+v, %v", r, err)
	}
	if !slices.Contains(w.ops, "delete task canary-1") || !slices.Contains(w.ops, "delete workspace canary-1") {
		t.Errorf("cancelled: ops %v", w.ops)
	}
	if arch.dir == "" || !strings.Contains(string(arch.files["meta.json"]), "context canceled") {
		t.Errorf("cancelled: archive %q: %s", arch.dir, arch.files["meta.json"])
	}
}

// What can't be read is noted, not fatal: usage, and a specialist's calls.
func TestRunChatUnreadable(t *testing.T) {
	w, s, run, o := newChatWorld(t)
	w.failUsage, w.failSubagent = true, true
	w.mainCalls = []string{"posture-audit", "lookout_k8s_audit_all", "leeway_report"}
	w.mainResults = append(w.mainResults, ToolResult{Tool: "leeway_report", Output: leewayOK})
	r, err := RunChat(context.Background(), s, w, w, run, o)
	if err != nil || !r.Pass || r.Usage != nil {
		t.Fatalf("result %+v, err %v", r, err)
	}
	notes := strings.Join(r.Trajectory.Notes, "\n")
	if !strings.Contains(notes, "specialist posture-audit: its calls were not read: agents: 500") || !strings.Contains(notes, "max_model_calls not checked") {
		t.Errorf("notes %q", notes)
	}
}

func TestRouterChat(t *testing.T) {
	var inject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ate-target-actor") != "agents/canary-1" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "who?", http.StatusForbidden)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /sessions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"app":"a","user":"u","sessionID":"s-9","url":"x"}`)
		case "POST /sessions/s-9/inject":
			b, _ := io.ReadAll(r.Body)
			inject = string(b)
			w.WriteHeader(http.StatusAccepted)
		case "GET /sessions/s-9/agents/posture-audit/events":
			if r.URL.Query().Get("limit") != "5000" {
				http.Error(w, "limit?", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"agent":"posture-audit","truncated":true,"events":[
				{"seq":4,"event":{"author":"posture-audit","content":{"role":"model","parts":[{"functionCall":{"name":"lookout_k8s_audit_all"}},{"functionCall":{"name":"leeway_report"}}]}}},
				{"seq":5,"event":{"author":"posture-audit","content":{"role":"user","parts":[
					{"functionResponse":{"name":"lookout_k8s_audit_all","response":{"output":"scanned=12"}}},
					{"functionResponse":{"name":"structured","response":{"output":{"n":1}}}},
					{"functionResponse":{"name":"posture-audit","response":{"title":"t"}}},
					{"functionResponse":{"name":"leeway_report","response":{"error":"Tool execution failed. Details: list nodes: Unauthorized"}}},
					{"functionResponse":{"name":"quiet","response":{"error":""}}},
					{"functionResponse":{"name":"odd","response":{"error":{"code":7}}}}]}}}]}`)
		case "GET /sessions/s-9/agents/_fallback/events":
			http.Error(w, `{"error":"no turns recorded"}`, http.StatusNotFound)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	rt := &Router{Base: srv.URL + "/", Atespace: "agents", Token: "tok"}
	ctx := context.Background()

	id, err := rt.CreateSession(ctx, "canary-1")
	if err != nil || id != "s-9" {
		t.Fatalf("CreateSession = %q, %v", id, err)
	}
	if err := rt.Inject(ctx, "canary-1", id, `review "demo"`); err != nil || inject != `{"message":"review \"demo\""}` {
		t.Fatalf("Inject: %v, body %s", err, inject)
	}
	tr, known, err := rt.SubagentEvents(ctx, "canary-1", id, "posture-audit")
	if err != nil || !known || !tr.Truncated {
		t.Fatalf("SubagentEvents: %v, known %v, transcript %+v", err, known, tr)
	}
	// A string output is unquoted; a structured one, and a tool that
	// isn't MCP's, are kept as JSON. A response whose error is empty
	// didn't fail, so it is a result too.
	if want := []ToolResult{{Tool: "lookout_k8s_audit_all", Output: "scanned=12"}, {Tool: "structured", Output: `{"n":1}`}, {Tool: "posture-audit", Output: `{"title":"t"}`}, {Tool: "quiet", Output: `{"error":""}`}}; !slices.Equal(tr.Results(), want) {
		t.Errorf("results %q, want %q", tr.Results(), want)
	}
	if !slices.Equal(tr.Calls(), []string{"lookout_k8s_audit_all", "leeway_report"}) {
		t.Errorf("calls %v", tr.Calls())
	}
	// An error result counts; an empty one doesn't; one that isn't a
	// string is kept as written.
	if want := []string{"leeway_report: Tool execution failed. Details: list nodes: Unauthorized", `odd: {"code":7}`}; !slices.Equal(tr.Failures(), want) {
		t.Errorf("failures %q, want %q", tr.Failures(), want)
	}
	if _, known, err := rt.SubagentEvents(ctx, "canary-1", id, "_fallback"); err != nil || known {
		t.Errorf("a subagent the agent doesn't have: known %v, err %v", known, err)
	}
	if _, _, err := rt.SubagentEvents(ctx, "canary-1", id, "other"); err == nil {
		t.Error("a failing read succeeded")
	}
	if err := rt.Inject(ctx, "canary-1", "nope", "x"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a failing inject: %v", err)
	}
	// A wrong token is the agent's refusal, reported as such.
	bad := &Router{Base: srv.URL, Atespace: "agents", Token: "wrong"}
	if _, err := bad.CreateSession(ctx, "canary-1"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a refused create: %v", err)
	}
	// An answer naming no session is an error, not an empty ID.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer empty.Close()
	if _, err := (&Router{Base: empty.URL, Atespace: "agents"}).CreateSession(ctx, "canary-1"); err == nil || !strings.Contains(err.Error(), "names no session") {
		t.Errorf("a create with no session: %v", err)
	}
}

// leewayReport is a `report` result as leeway v0.1.8 writes it: compact
// JSON, one entry a cluster, with a failed cluster inside a result that
// is not an error.
func leewayReport(status string, succeeded, failed int) string {
	return fmt.Sprintf(`{"schema_version":"1","clusters":[{"cluster":"demo","status":%q}],"summary":{"clusters_total":1,"clusters_succeeded":%d,"clusters_failed":%d}}`, status, succeeded, failed)
}

// The posture scenario's result checks, against a report in each shape
// core-agent records one: as the MCP toolset returns it, wrapped in a
// digest whole, wrapped in a digest pruned (the tool's JSON text parsed
// into an object), and summarized by a model.
func TestPostureResultsInRecordedShapes(t *testing.T) {
	s, err := Load("../../e2e/scenarios/posture-audit")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	plain := func(report string) string { return `{"output":` + quote(report) + `}` }
	whole := func(report string) string {
		return `{"digest":` + quote(plain(report)) + `,"raw_bytes":300,"method":"passthrough","latency_ms":12}`
	}
	pruned := func(report string) string {
		return `{"digest":` + quote(`{"output":`+report+`}`) + `,"raw_bytes":9000,"method":"structural_json","latency_ms":12}`
	}
	bad, good := leewayReport("error", 0, 1), leewayReport("ok", 1, 0)
	for _, tc := range []struct {
		name, response string
		method         string
		want           int
	}{
		{"plain, unreachable", plain(bad), "", 3},
		{"plain, reached", plain(good), "", 0},
		{"digest whole, unreachable", whole(bad), "passthrough", 3},
		{"digest whole, reached", whole(good), "passthrough", 0},
		{"digest pruned, unreachable", pruned(bad), "structural_json", 3},
		{"digest pruned, reached", pruned(good), "structural_json", 0},
		// A model's summary can't be held to the tool's own fields.
		{"summarized", `{"digest":"The report covers demo; nothing failed.","method":"llm_fallback"}`, "llm_fallback", 1},
		// A digest that isn't a tool's output object is kept as it is.
		{"digest of something else", `{"digest":"{\"n\":1}","method":"passthrough"}`, "passthrough", 0},
		{"digest cut short", `{"digest":"{\"output\":\"{\\\"sche…<9 more bytes>","method":"passthrough"}`, "passthrough", 0},
		// A response that is not an object is still a result.
		{"not an object", `"done"`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := `data: {"seq":1,"event":{"author":"posture-audit","content":{"role":"user","parts":[{"text":"kept"},{"functionResponse":{"name":"leeway_report","response":` + tc.response + `}}]}}}` + "\n\n"
			tr, err := ParseEvents(strings.NewReader(stream))
			if err != nil {
				t.Fatal(err)
			}
			results := tr.Results()
			if len(results) != 1 || results[0].Method != tc.method || len(tr.Events) != 1 || !slices.Equal(tr.Events[0].Texts, []string{"kept"}) {
				t.Fatalf("results %+v, events %+v", results, tr.Events)
			}
			if got := s.CheckResults(results); len(got) != tc.want {
				t.Errorf("failures %q, want %d (output %q)", got, tc.want, results[0].Output)
			}
		})
	}
}

// A long tool error is cut, so a result stays readable.
func TestEventFailureIsBounded(t *testing.T) {
	long := strings.Repeat("x", 2*maxFailure)
	tr, err := ParseEvents(strings.NewReader(`data: {"seq":1,"event":{"author":"a","content":{"role":"user","parts":[{"functionResponse":{"name":"t","response":{"error":"` + long + `"}}}]}}}` + "\n"))
	if err != nil || len(tr.Failures()) != 1 {
		t.Fatalf("failures %v, err %v", tr.Failures(), err)
	}
	if got := tr.Failures()[0]; !strings.HasPrefix(got, "t: xxx") || !strings.HasSuffix(got, "…") || len(got) > maxFailure+10 {
		t.Errorf("failure of %d bytes: %.40s…", len(got), got)
	}
}

func TestAxChatCommands(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ax")
	script := "#!/bin/sh\necho \"$*\" >> \"$(dirname \"$0\")/args\"\ncase \"$*\" in *apply*) cat >> \"$(dirname \"$0\")/stdin\";; *fail*) echo boom >&2; exit 1;; esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	ctx := context.Background()
	a := Ax{Bin: bin, Server: "ax:8080", Atespace: "agents"}
	if err := a.Apply(ctx, []byte(chatTask)); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteWorkspace(ctx, "canary-1"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.Contains(string(args), "--server ax:8080 -a agents apply -f -") || !strings.Contains(string(args), "-a agents delete workspace canary-1") || string(stdin) != chatTask {
		t.Errorf("ax args:\n%s\nstdin:\n%s", args, stdin)
	}
	if err := a.DeleteWorkspace(ctx, "fail"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failing ax: err = %v", err)
	}
	if err := (Ax{Atespace: "agents"}).Apply(ctx, []byte(chatTask)); err == nil {
		t.Error("ax with no connection applied")
	}
	if err := (Ax{Bin: filepath.Join(dir, "missing"), Server: "ax:8080", Atespace: "agents"}).Apply(ctx, []byte(chatTask)); err == nil {
		t.Error("a missing ax applied")
	}
}

func TestLoadChat(t *testing.T) {
	s, err := Load("../../e2e/scenarios/posture-audit")
	if err != nil {
		t.Fatal(err)
	}
	if s.Chat == nil || !slices.Equal(s.Chat.Specialists, []string{"posture-audit"}) || s.Limits.Start != 5*time.Minute {
		t.Fatalf("scenario %+v", s)
	}
	c := s.ForCluster(chatClusterPath)
	ask, err := c.Ask("run-1")
	if err != nil || !strings.Contains(ask, chatClusterPath) {
		t.Errorf("Ask = %q, %v", ask, err)
	}
	if missing, _ := c.Score("demo is fine", "run-1"); len(missing) != 0 {
		t.Errorf("missing %v", missing)
	}
	// The marker the question asks for, at the start of a line; not a
	// good answer saying there were no failures.
	for answer, want := range map[string]int{
		"TOOL FAILURE: leeway_report":                    1,
		"Summary\n\n**TOOL FAILURE**\nleeway_report":     1,
		"No tool failures occurred; demo is healthy.":    0,
		"Both tools ran. There was no TOOL FAILURE.":     0,
		"Tool failure handling is configured correctly.": 0,
	} {
		if got, _ := c.Unwanted(answer, "run-1"); len(got) != want {
			t.Errorf("answer %q: unwanted %v, want %d", answer, got, want)
		}
	}
	// ForCluster copies: the loaded scenario still renders for no cluster.
	if ask, _ := s.Ask("run-1"); strings.Contains(ask, chatClusterPath) {
		t.Error("ForCluster changed the scenario it was called on")
	}
	// The incident runner can't run it: there is no workload to break.
	if r, err := Run(context.Background(), s, nil, nil, nil, Options{}); r != nil || err == nil {
		t.Errorf("Run on a chat scenario: %+v, %v", r, err)
	}

	const head = "expect:\n  mentions: [x]\n"
	for _, tc := range []struct{ name, body, want string }{
		{"no question", "chat: {}\n" + head + "limits: {start: 1m, diagnose: 1m}\n", "chat.ask: want the question to send"},
		{"bad question", "chat: {ask: '{{.Nope}}'}\n" + head + "limits: {start: 1m, diagnose: 1m}\n", "chat.ask:"},
		{"a workload", "chat: {ask: hi}\nnamespace: triage-lab\n" + head + "limits: {start: 1m, diagnose: 1m}\n", "a chat scenario breaks no workload"},
		{"bad specialist", "chat: {ask: hi, specialists: ['Not A Name']}\n" + head + "limits: {start: 1m, diagnose: 1m}\n", "chat.specialists"},
		{"no start limit", "chat: {ask: hi}\n" + head + "limits: {diagnose: 1m}\n", "a chat scenario wants start and diagnose"},
		{"incident limits", "chat: {ask: hi}\n" + head + "limits: {start: 1m, diagnose: 1m, suspend: 1m}\n", "incident and suspend are for an incident scenario"},
		{"bad absent", "chat: {ask: hi}\nexpect:\n  mentions: [x]\n  absent: ['(']\nlimits: {start: 1m, diagnose: 1m}\n", "expect.absent"},
		{"result check without patterns", "chat: {ask: hi}\nexpect:\n  mentions: [x]\n  results: [{tool: report}]\nlimits: {start: 1m, diagnose: 1m}\n", "each entry wants a tool and at least one absent pattern"},
		{"bad result pattern", "chat: {ask: hi}\nexpect:\n  mentions: [x]\n  results: [{tool: report, absent: ['(']}]\nlimits: {start: 1m, diagnose: 1m}\n", "expect.results report"},
		{"result checks on an incident scenario", "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [x]\n  results: [{tool: report, absent: [x]}]\nlimits: {incident: 1m, diagnose: 1m, suspend: 1m}\n", "only a chat scenario checks tool results"},
		{"absences on an incident scenario", "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [x]\n  absent: [x]\nlimits: {incident: 1m, diagnose: 1m, suspend: 1m}\n", "only a chat scenario checks what the answer must not say"},
		{"a start limit on an incident scenario", "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [x]\nlimits: {incident: 1m, diagnose: 1m, suspend: 1m, start: 1m}\n", "start is for a chat scenario"},
	} {
		dir := filepath.Join(t.TempDir(), "chat-case")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scenario.yaml"), []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}
