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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// world fakes the cluster, ax and the agent on a virtual clock: after
// incidentAt the incident task exists; after answerAt the agent has
// answered; once the workload is removed, the task suspends after
// suspendAfter.
type world struct {
	now          time.Time
	scenario     *Scenario
	name         string
	applied      []string
	deleted      []string
	tasksGone    []string
	incidentAt   time.Duration
	answerAt     time.Duration
	answer       string
	suspendAfter time.Duration
	removedAt    time.Time
	wokeStatus   string
	wokeEvents   int
	failApply    bool
	neverIdle    bool
	ops          []string
	leftover     bool
	axFlaky      int
	swept        []string
	onCleanup    func()
	failTasks    bool
	failDelete   bool
}

func (w *world) Apply(_ context.Context, m []byte, _ bool) (string, error) {
	if w.failApply {
		return "", errors.New("apply failed")
	}
	w.applied = append(w.applied, string(m))
	return "", nil
}

func (w *world) Delete(ctx context.Context, m []byte) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	w.deleted = append(w.deleted, string(m))
	w.ops = append(w.ops, "delete workload")
	if w.onCleanup != nil {
		w.onCleanup()
	}
	if w.removedAt.IsZero() {
		w.removedAt = w.now
	}
	return "", nil
}

func (w *world) since() time.Duration { return w.now.Sub(t0) }

func (w *world) DeleteLabeled(_ context.Context, ns, kinds, sel string) (string, error) {
	w.swept = append(w.swept, ns+" "+kinds+" "+sel)
	return "", nil
}

func (w *world) Tasks(context.Context) (map[string]string, error) {
	if w.failTasks {
		return nil, errors.New("ax down")
	}
	if w.axFlaky > 0 && !w.removedAt.IsZero() {
		w.axFlaky--
		return nil, errors.New("ax blip")
	}
	tasks := map[string]string{"lookout-old": "Suspended", "demo-agent-1": "Suspended"}
	if w.leftover && !contains(w.tasksGone, "lookout-left") {
		tasks["lookout-left"] = "Running"
	}
	if w.incidentAt >= 0 && w.since() >= w.incidentAt {
		phase := "Running"
		if !w.removedAt.IsZero() && w.suspendAfter >= 0 && w.now.Sub(w.removedAt) >= w.suspendAfter {
			phase = "Suspended"
		}
		tasks["lookout-new"] = phase
	}
	return tasks, nil
}

func (w *world) DeleteTask(_ context.Context, name string) error {
	if w.failDelete {
		return errors.New("delete failed")
	}
	w.tasksGone = append(w.tasksGone, name)
	w.ops = append(w.ops, "delete task "+name)
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func (w *world) suspended() bool {
	tasks, _ := w.Tasks(context.Background())
	return tasks["lookout-new"] == "Suspended"
}

func (w *world) Sessions(_ context.Context, task string) ([]Session, error) {
	if task != "lookout-new" && task != "lookout-old" && task != "lookout-left" {
		return nil, errors.New("not found")
	}
	status := "active"
	if task == "lookout-new" && w.suspended() && w.wokeStatus != "" {
		status = w.wokeStatus
	}
	return []Session{{ID: "s-" + task, Status: status}}, nil
}

func (w *world) State(context.Context, string, string) (string, error) {
	if !w.neverIdle && w.answerAt >= 0 && w.since() >= w.answerAt {
		return "idle", nil
	}
	return "running", nil
}

func (w *world) Events(_ context.Context, task, _ string) (*Transcript, error) {
	if task == "lookout-old" {
		return &Transcript{Events: []Event{{Seq: 1, Author: "user", Texts: []string{"old incident"}}}}, nil
	}
	if task == "lookout-left" {
		return &Transcript{Events: []Event{{Seq: 1, Author: "user", Texts: []string{`{"name":"missing-env-1019-031700-abc"}`}}}}, nil
	}
	tr := &Transcript{Cost: 0.05, Events: []Event{{Seq: 3, Author: "user", Texts: []string{`{"name":"` + w.name + `"}`}}}}
	if w.answerAt >= 0 && w.since() >= w.answerAt {
		tr.Events = append(tr.Events, Event{Seq: 29, Author: "gke-triage_coordinator", Role: "model", Texts: []string{w.answer}})
	}
	if w.suspended() && w.wokeEvents > 0 {
		tr.Events = tr.Events[:min(w.wokeEvents, len(tr.Events))]
	}
	return tr, nil
}

func newWorld(t *testing.T) (*world, *Scenario, Options) {
	t.Helper()
	s, err := Load("../../e2e/scenarios/missing-env")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{now: t0, scenario: s, name: RunName(s.Name, t0),
		incidentAt: 2 * time.Minute, answerAt: 5 * time.Minute, suspendAfter: 11 * time.Minute}
	w.answer = fmt.Sprintf("DATABASE_URL is missing from ConfigMap %s-config.", w.name)
	o := Options{
		Poll:  30 * time.Second,
		Now:   func() time.Time { return w.now },
		Sleep: func(_ context.Context, d time.Duration) error { w.now = w.now.Add(d); return nil },
	}
	return w, s, o
}

func run(t *testing.T, w *world, s *Scenario, o Options) *Result {
	t.Helper()
	r, err := Run(context.Background(), s, w, w, w, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

func TestRunPasses(t *testing.T) {
	w, s, o := newWorld(t)
	var logs []string
	o.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	r := run(t, w, s, o)
	if !r.Pass || !r.Correct || !r.Suspended || !r.Woke || r.Task != "lookout-new" || r.CostUSD != 0.05 {
		t.Fatalf("result %+v", r)
	}
	if len(w.applied) != 1 || !strings.Contains(w.applied[0], "name: "+w.name+"\n") || !strings.Contains(w.applied[0], "namespace: triage-lab") {
		t.Errorf("applied %v", w.applied)
	}
	if len(w.deleted) != 1 || len(w.tasksGone) != 1 || w.tasksGone[0] != "lookout-new" {
		t.Errorf("cleanup: deleted %d workloads, tasks %v", len(w.deleted), w.tasksGone)
	}
	if r.IncidentAfter != "2m0s" || r.SuspendAfter == "" || len(logs) == 0 {
		t.Errorf("timings %+v, logs %v", r, logs)
	}
}

func TestRunFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*world)
		want   string
	}{
		{"no incident", func(w *world) { w.incidentAt = -1 }, "no lookout incident task"},
		{"no answer", func(w *world) { w.answerAt = -1 }, "no settled answer within"},
		// The agent wrote the right words but never finished: not a pass.
		{"unsettled answer", func(w *world) { w.neverIdle = true }, "no settled answer within"},
		{"wrong answer", func(w *world) { w.answer = "The pod is crash-looping." }, "the answer lacks DATABASE_URL"},
		{"no suspend", func(w *world) { w.suspendAfter = -1 }, "did not suspend"},
		{"restarted", func(w *world) { w.wokeStatus = "idle" }, "mast restarted instead of resuming"},
		{"lost history", func(w *world) { w.wokeEvents = 1 }, "lost history"},
		{"cleanup fails", func(w *world) { w.failDelete = true }, "deleting task lookout-new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s, o := newWorld(t)
			tc.mutate(w)
			r := run(t, w, s, o)
			if r.Pass || !strings.Contains(strings.Join(r.Failures, "; "), tc.want) {
				t.Errorf("pass=%v failures %v, want %q", r.Pass, r.Failures, tc.want)
			}
			if len(w.deleted) != 1 {
				t.Errorf("workload not removed after a failed run (%d deletes)", len(w.deleted))
			}
		})
	}
}

func TestRunKeep(t *testing.T) {
	w, s, o := newWorld(t)
	o.Keep = true
	r := run(t, w, s, o)
	if !r.Pass || r.Suspended || len(w.deleted) != 0 || len(w.tasksGone) != 0 {
		t.Errorf("keep: %+v, deleted %d, tasks %v", r, len(w.deleted), w.tasksGone)
	}
}

func TestRunErrors(t *testing.T) {
	w, s, o := newWorld(t)
	w.failApply = true
	if _, err := Run(context.Background(), s, w, w, w, o); err == nil {
		t.Error("Run with a failing apply succeeded")
	}
	w, s, o = newWorld(t)
	w.failTasks = true
	if _, err := Run(context.Background(), s, w, w, w, o); err == nil {
		t.Error("Run with ax down succeeded")
	}
	w, s, o = newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	if _, err := Run(ctx, s, w, w, w, o); err == nil {
		t.Error("Run with a cancelled context succeeded")
	}
}

func TestParseTasks(t *testing.T) {
	out := "NAME                ATESPACE    PHASE       ACTOR      WORKER-IP   AGE\nlookout-1   cred-test   Suspended   lookout-1  <none>   1d\ndemo-2   cred-test   Running   demo-2  10.0.0.1   2h\n"
	tasks, err := ParseTasks(out)
	if err != nil || tasks["lookout-1"] != "Suspended" || tasks["demo-2"] != "Running" || len(tasks) != 2 {
		t.Errorf("ParseTasks = %v, %v", tasks, err)
	}
	if tasks, err := ParseTasks(""); err != nil || len(tasks) != 0 {
		t.Errorf("empty = %v, %v", tasks, err)
	}
	if _, err := ParseTasks("WHAT EVER\nx y\n"); err == nil {
		t.Error("bad header accepted")
	}
}

func TestAxCommands(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ax")
	script := "#!/bin/sh\necho \"$*\" >> \"$(dirname \"$0\")/args\"\ncase \"$*\" in *'get tasks'*) printf 'NAME ATESPACE PHASE\\nlookout-1 a Running\\n';; *fail*) echo boom >&2; exit 1;; esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	ctx := context.Background()
	a := Ax{Bin: bin, Server: "ax:8080", Atespace: "agents"}
	if tasks, err := a.Tasks(ctx); err != nil || tasks["lookout-1"] != "Running" {
		t.Errorf("Tasks = %v, %v", tasks, err)
	}
	if err := (Ax{Bin: bin, Context: "demo", Atespace: "agents"}).DeleteTask(ctx, "lookout-1"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(b), "--server ax:8080 -a agents get tasks") || !strings.Contains(string(b), "--context demo -a agents delete task lookout-1") {
		t.Errorf("ax args:\n%s", b)
	}
	if err := (Ax{Bin: bin, Context: "demo", Atespace: "agents"}).DeleteTask(ctx, "fail"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failing ax: err = %v", err)
	}
	if _, err := (Ax{Atespace: "agents"}).Tasks(ctx); err == nil {
		t.Error("ax with no connection succeeded")
	}
}

// Cleanup removes the workload before the task: while the workload stays
// broken, lookout keeps sending and would recreate a deleted task.
func TestRunCleanupOrder(t *testing.T) {
	w, s, o := newWorld(t)
	w.answerAt = -1 // fail early, through the deferred cleanup
	run(t, w, s, o)
	if got := strings.Join(w.ops, ", "); got != "delete workload, delete task lookout-new" {
		t.Errorf("cleanup order %q", got)
	}
}

// A cancelled run (SIGTERM, a Job's deadline) still cleans up.
func TestRunCancelledCleansUp(t *testing.T) {
	w, s, o := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	o.Sleep = func(ctx context.Context, d time.Duration) error {
		w.now = w.now.Add(d)
		if w.since() >= 6*time.Minute {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	r, err := Run(ctx, s, w, w, w, o)
	if err == nil || r == nil || r.Pass {
		t.Fatalf("cancelled run: %+v, %v", r, err)
	}
	if len(w.deleted) != 1 || !contains(w.tasksGone, "lookout-new") {
		t.Errorf("cancelled run left things behind: %d deletes, tasks %v", len(w.deleted), w.tasksGone)
	}
}

// A task that appears only after the incident wait gave up is found by
// cleanup's own search and deleted.
func TestRunDeletesLateTask(t *testing.T) {
	w, s, o := newWorld(t)
	w.incidentAt = 10*time.Minute + 15*time.Second // just after the 10m wait gives up
	o.Sleep = func(_ context.Context, d time.Duration) error { w.now = w.now.Add(d); return nil }
	w.onCleanup = func() { w.now = w.now.Add(time.Minute) }
	r := run(t, w, s, o)
	if r.Pass || r.Task != "lookout-new" || !contains(w.tasksGone, "lookout-new") {
		t.Errorf("late task: pass=%v task=%q deleted %v", r.Pass, r.Task, w.tasksGone)
	}
}

// Each run first sweeps what killed runs left: labeled workloads, and
// lookout tasks naming an earlier run's workload.
func TestRunSweepsLeftovers(t *testing.T) {
	w, s, o := newWorld(t)
	w.leftover = true
	if r := run(t, w, s, o); !r.Pass {
		t.Fatalf("run failed: %v", r.Failures)
	}
	if len(w.swept) != 1 || w.swept[0] != "triage-lab deployments,configmaps shipmind.dev/scenario=missing-env" {
		t.Errorf("swept %v", w.swept)
	}
	if !contains(w.tasksGone, "lookout-left") || contains(w.tasksGone, "lookout-old") {
		t.Errorf("tasks deleted %v, want lookout-left and not lookout-old", w.tasksGone)
	}
}

// A moment of ax being unreachable while waiting to suspend is retried.
func TestRunRetriesAxBlips(t *testing.T) {
	w, s, o := newWorld(t)
	w.axFlaky = 3
	if r := run(t, w, s, o); !r.Pass || !r.Suspended {
		t.Errorf("run with ax blips: %+v", r)
	}
}

func TestMentionsRun(t *testing.T) {
	tr := &Transcript{Events: []Event{{Texts: []string{"pod missing-env-config is fine; missing-env-1019-031700-abc crashes"}}}}
	if !tr.MentionsRun("missing-env-") {
		t.Error("MentionsRun missed a run workload")
	}
	tr = &Transcript{Events: []Event{{Texts: []string{"missing-env-config and missing-env-12-3"}}}}
	if tr.MentionsRun("missing-env-") {
		t.Error("MentionsRun matched a non-run name")
	}
}
