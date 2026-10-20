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
	tasks := map[string]string{"lookout-00000000000000aa": "Suspended", "demo-agent-1": "Suspended", "lookout-wb-demo-1234abcd": "Running",
		"lookout-2222222222222222": "Running"}
	if w.leftover && !contains(w.tasksGone, "lookout-1111111111111111") {
		tasks["lookout-1111111111111111"] = "Running"
	}
	if w.incidentAt >= 0 && w.since() >= w.incidentAt {
		phase := "Running"
		if !w.removedAt.IsZero() && w.suspendAfter >= 0 && w.now.Sub(w.removedAt) >= w.suspendAfter {
			phase = "Suspended"
		}
		tasks["lookout-0123456789abcdef"] = phase
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
	return tasks["lookout-0123456789abcdef"] == "Suspended"
}

func (w *world) Sessions(_ context.Context, task string) ([]Session, error) {
	if task != "lookout-0123456789abcdef" && task != "lookout-00000000000000aa" && task != "lookout-1111111111111111" && task != "lookout-wb-demo-1234abcd" && task != "lookout-2222222222222222" {
		return nil, errors.New("not found")
	}
	status := "active"
	if task == "lookout-0123456789abcdef" && w.suspended() && w.wokeStatus != "" {
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
	if task == "lookout-00000000000000aa" {
		return &Transcript{Events: []Event{{Seq: 1, Author: "user", Texts: []string{"old incident"}}}}, nil
	}
	if task == "lookout-2222222222222222" {
		// A storm on a shared Node: long-lived, its members include an
		// earlier run's pod and this run's.
		return &Transcript{Events: []Event{{Seq: 1, Author: "user", Texts: []string{
			`{"kind":"storm","namespace":"triage-lab","ancestor_name":"node-1","representative_incidents":["missing-env-1019-031700-7d9f8c-abcde","` + w.name + `-64544d6c4b-x58zg"]}`}}}}, nil
	}
	if task == "lookout-wb-demo-1234abcd" {
		// The watchboard: a long-lived agent that hears about scenario
		// workloads (an earlier run's and this one's) in later messages.
		return &Transcript{Events: []Event{
			{Seq: 1, Author: "user", Texts: []string{"digest: cluster healthy"}},
			{Seq: 2, Author: "user", Texts: []string{`resolved probe_flap missing-env-1019-031700-abc and ` + w.name}},
		}}, nil
	}
	if task == "lookout-1111111111111111" {
		return &Transcript{Events: []Event{{Seq: 1, Author: "user", Texts: []string{
			`{"kind":"k8s-event","namespace":"triage-lab","name":"missing-env-1019-031700-7d9f8c-abcde"}`}}}}, nil
	}
	tr := &Transcript{Cost: 0.05, Events: []Event{{Seq: 3, Author: "user", Texts: []string{
		`{"kind":"k8s-event","namespace":"triage-lab","name":"` + w.name + `-64544d6c4b-x58zg"}`}}}}
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
	if !r.Pass || !r.Correct || !r.Suspended || !r.Woke || r.Task != "lookout-0123456789abcdef" || r.CostUSD != 0.05 {
		t.Fatalf("result %+v", r)
	}
	if len(w.applied) != 1 || !strings.Contains(w.applied[0], "name: "+w.name+"\n") || !strings.Contains(w.applied[0], "namespace: triage-lab") {
		t.Errorf("applied %v", w.applied)
	}
	if len(w.deleted) != 1 || len(w.tasksGone) != 1 || w.tasksGone[0] != "lookout-0123456789abcdef" {
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
		{"cleanup fails", func(w *world) { w.failDelete = true }, "deleting task lookout-0123456789abcdef"},
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
	if got := strings.Join(w.ops, ", "); got != "delete workload, delete task lookout-0123456789abcdef" {
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
	if len(w.deleted) != 1 || !contains(w.tasksGone, "lookout-0123456789abcdef") {
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
	if r.Pass || r.Task != "lookout-0123456789abcdef" || !contains(w.tasksGone, "lookout-0123456789abcdef") {
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
	if !contains(w.tasksGone, "lookout-1111111111111111") || contains(w.tasksGone, "lookout-00000000000000aa") {
		t.Errorf("tasks deleted %v, want lookout-1111111111111111 and not lookout-00000000000000aa", w.tasksGone)
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
	if !mentionsRun("pod missing-env-config is fine; missing-env-1019-031700-abc crashes", "missing-env-") {
		t.Error("mentionsRun missed a run workload")
	}
	if mentionsRun("missing-env-config and missing-env-12-3", "missing-env-") {
		t.Error("mentionsRun matched a non-run name")
	}
}

// lookout's watchboard, cluster-scope and storm tasks hear about
// scenario workloads too; they are never swept, picked or deleted. (A
// sweep that took "any lookout task mentioning a run" deleted the
// watchboard once.)
func TestRunNeverTouchesTheWatchboard(t *testing.T) {
	w, s, o := newWorld(t)
	w.leftover = true
	w.incidentAt = -1 // only the watchboard mentions this run's workload
	r := run(t, w, s, o)
	for _, shared := range []string{"lookout-wb-demo-1234abcd", "lookout-2222222222222222"} {
		if contains(w.tasksGone, shared) || r.Task == shared {
			t.Errorf("shared task %s was taken for a run's task: deleted %v, task %q", shared, w.tasksGone, r.Task)
		}
	}
	if !contains(w.tasksGone, "lookout-1111111111111111") {
		t.Errorf("the real leftover incident task was not swept: %v", w.tasksGone)
	}
}

func TestFirstMessage(t *testing.T) {
	tr := &Transcript{Events: []Event{
		{Author: "agent", Texts: []string{"hello"}},
		{Author: "user", Texts: []string{"incident", "detail"}},
		{Author: "user", Texts: []string{"later"}},
	}}
	if got := tr.FirstMessage(); got != "incident\ndetail" {
		t.Errorf("FirstMessage = %q", got)
	}
	if got := (&Transcript{}).FirstMessage(); got != "" {
		t.Errorf("empty FirstMessage = %q", got)
	}
}

func TestFirstIncident(t *testing.T) {
	msg := func(m string) *Transcript { return &Transcript{Events: []Event{{Author: "user", Texts: []string{m}}}} }
	in, ok := msg(`{"kind":"k8s-event","namespace":"lab","name":"w-1-2"}`).FirstIncident()
	if !ok || !in.About("lab", "w") || in.About("other", "w") || in.About("lab", "w-1-2-3") {
		t.Errorf("incident %+v, %v", in, ok)
	}
	for _, m := range []string{`{"kind":"storm","name":"x"}`, `not json`, `{"kind":"k8s-event"}`, `{"name":"x"}`} {
		if _, ok := msg(m).FirstIncident(); ok {
			t.Errorf("FirstIncident(%s) = ok", m)
		}
	}
	if (Incident{Namespace: "lab", Name: "w"}).About("lab", "w") != true {
		t.Error("the workload itself is not About it")
	}
	if (Incident{Namespace: "lab", Name: "wx-1"}).About("lab", "w") {
		t.Error("a workload sharing a prefix matched")
	}
}
