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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/judge"
)

// Kube applies and deletes manifests (cluster.Kube satisfies it).
type Kube interface {
	Apply(ctx context.Context, manifests []byte, dryRun bool) (string, error)
	Delete(ctx context.Context, manifests []byte) (string, error)
}

// Tasks lists and deletes ax tasks.
type Tasks interface {
	// Tasks returns task name to phase ("Running", "Suspended", ...).
	Tasks(ctx context.Context) (map[string]string, error)
	DeleteTask(ctx context.Context, name string) error
}

// Agent reads a task's sessions (Router satisfies it).
type Agent interface {
	Sessions(ctx context.Context, task string) ([]Session, error)
	State(ctx context.Context, task, session string) (string, error)
	Events(ctx context.Context, task, session string) (*Transcript, error)
	Usage(ctx context.Context, task, session string) (*AgentUsage, error)
}

// Ax runs the ax CLI.
type Ax struct {
	// Bin is the binary; empty means "ax".
	Bin string
	// Server is ax-server's address, for running inside the cluster;
	// otherwise Context is the kube context ax tunnels through.
	Server, Context string
	Atespace        string
}

func (a Ax) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	bin := a.Bin
	if bin == "" {
		bin = "ax"
	}
	var conn []string
	switch {
	case a.Server != "":
		conn = []string{"--server", a.Server}
	case a.Context != "":
		conn = []string{"--context", a.Context}
	default:
		return nil, errors.New("ax: neither a server nor a kube context")
	}
	return exec.CommandContext(ctx, bin, append(append(conn, "-a", a.Atespace), args...)...), nil //nolint:gosec // fixed binary and arguments
}

func (a Ax) run(ctx context.Context, args ...string) (string, error) {
	cmd, err := a.command(ctx, args...)
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ax %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Tasks implements Tasks by parsing `ax get tasks`.
func (a Ax) Tasks(ctx context.Context) (map[string]string, error) {
	out, err := a.run(ctx, "get", "tasks")
	if err != nil {
		return nil, err
	}
	return ParseTasks(out)
}

// ParseTasks reads `ax get tasks` output: a header row naming NAME and
// PHASE columns, then one row per task.
func ParseTasks(out string) (map[string]string, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	tasks := map[string]string{}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return tasks, nil
	}
	header := strings.Fields(lines[0])
	name, phase := slices.Index(header, "NAME"), slices.Index(header, "PHASE")
	if name < 0 || phase < 0 {
		return nil, fmt.Errorf("ax get tasks: unexpected header %q", lines[0])
	}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) > max(name, phase) {
			tasks[f[name]] = f[phase]
		}
	}
	return tasks, nil
}

// TaskWorkers implements TaskInspector from `ax get tasks`' WORKER-IP column.
func (a Ax) TaskWorkers(ctx context.Context) (map[string]string, error) {
	out, err := a.run(ctx, "get", "tasks")
	if err != nil {
		return nil, err
	}
	return parseTaskWorkers(out)
}

func parseTaskWorkers(out string) (map[string]string, error) {
	workers := map[string]string{}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return workers, nil
	}
	header := strings.Fields(lines[0])
	name, ip := slices.Index(header, "NAME"), slices.Index(header, "WORKER-IP")
	if name < 0 || ip < 0 {
		return nil, fmt.Errorf("ax get tasks: no NAME and WORKER-IP columns in %q", lines[0])
	}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) > max(name, ip) && f[ip] != "<none>" {
			workers[f[name]] = f[ip]
		}
	}
	return workers, nil
}

// TaskConditions implements TaskInspector from `ax get task -o yaml`.
func (a Ax) TaskConditions(ctx context.Context, task string) (map[string]Condition, error) {
	// ax prints a task as YAML.
	out, err := a.run(ctx, "get", "task", task)
	if err != nil {
		return nil, err
	}
	return parseTaskConditions(out)
}

func parseTaskConditions(out string) (map[string]Condition, error) {
	var doc struct {
		Status struct {
			Conditions []struct {
				Type, Status, Reason, Message string
				UpdateTime                    time.Time `yaml:"updateTime"`
			} `yaml:"conditions"`
		} `yaml:"status"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("ax get task: %w", err)
	}
	conds := map[string]Condition{}
	for _, c := range doc.Status.Conditions {
		conds[c.Type] = Condition{Status: c.Status, Reason: c.Reason, Message: c.Message, Updated: c.UpdateTime}
	}
	return conds, nil
}

// DeleteTask implements Tasks.
func (a Ax) DeleteTask(ctx context.Context, name string) error {
	_, err := a.run(ctx, "delete", "task", name)
	return err
}

// Result is one run's outcome, written as JSON.
type Result struct {
	Scenario string `json:"scenario"`
	Workload string `json:"workload"`
	Task     string `json:"task,omitempty"`
	Session  string `json:"session,omitempty"`

	IncidentAfter string `json:"incident_after,omitempty"`
	AnswerAfter   string `json:"answer_after,omitempty"`
	SuspendAfter  string `json:"suspend_after,omitempty"`

	Answer  string   `json:"answer,omitempty"`
	Missing []string `json:"missing,omitempty"`
	// Cluster is the cluster a chat run asked about, and Unwanted the
	// expect.absent patterns its answer matched.
	Cluster  string   `json:"cluster,omitempty"`
	Unwanted []string `json:"unwanted,omitempty"`
	CostUSD  float64  `json:"cost_usd"`
	// Superseded: a later message (lookout re-sending the incident)
	// arrived before the answer settled, so Usage may include calls
	// made for it.
	Superseded bool `json:"superseded,omitempty"`
	// Trajectory is the path the agent took to its answer and how it
	// fared against the scenario's expect.trajectory (#89).
	Trajectory *TrajectoryResult `json:"trajectory,omitempty"`
	// Usage is the session's model usage once the answer settled, in
	// one meaning whichever agent answered: model calls, tokens and the
	// cached share, cost per call. Absent when the read failed, which
	// doesn't fail the run.
	Usage *Usage `json:"usage,omitempty"`
	// Drain is what a worker drain did to the task, for a scenario with a
	// drain block (#105).
	Drain *DrainResult `json:"drain,omitempty"`
	// Storm is set when lookout folded the scenario workload into a shared
	// storm task instead of opening a dedicated per-incident task (#132).
	// Storm tasks are shared across workloads, so the runner reports the
	// fold and never grades, drains, or deletes the storm task.
	Storm *StormMatch `json:"storm,omitempty"`
	// Judge is a model's grade of the answer against the scenario's
	// expect.diagnosis (#90), when the run had a judge: reported, never a
	// failure, until there is a baseline to gate on.
	Judge *judge.Grade `json:"judge,omitempty"`

	Correct bool `json:"correct"`
	// Suspended: the task suspended once the workload was gone.
	Suspended bool `json:"suspended"`
	// Woke: the session answered again through the router, still in
	// the agent's memory (status "active") with its history intact.
	Woke bool `json:"woke"`

	Pass     bool     `json:"pass"`
	Failures []string `json:"failures,omitempty"`
}

// TrajectoryResult is a run's tool calls, in order, and what checking
// them found.
type TrajectoryResult struct {
	ToolCalls  []string `json:"tool_calls"`
	ModelCalls int      `json:"model_calls,omitempty"`
	// ToolErrors are the tool results that were errors, for a chat run,
	// which fails on one whose tool never succeeded.
	ToolErrors []string `json:"tool_errors,omitempty"`
	Failures   []string `json:"failures,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// DrainResult is a worker drain's effect on the task.
type DrainResult struct {
	// WorkerPod is the pod deleted, the one hosting the task's actor.
	WorkerPod string `json:"worker_pod,omitempty"`
	// Reason and Message are the task's AutoSuspended condition after it.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// After is the time from the pod's deletion to the suspend.
	After string `json:"after,omitempty"`
	// Neighbours are the other tasks that were on the worker, in the
	// runner's atespace, and how each came through the drain.
	Neighbours []NeighbourResult `json:"neighbours,omitempty"`
}

// NeighbourResult is how another task on the drained worker fared: the
// scenario's reason (WorkerDraining), resumed before it was seen
// suspended, or what went wrong.
type NeighbourResult struct {
	Task    string `json:"task"`
	Outcome string `json:"outcome"`
}

// Condition is one of a task's status conditions.
type Condition struct {
	Status, Reason, Message string
	// Updated is when ax last wrote it.
	Updated time.Time
}

// TaskInspector reads where a task runs and its conditions (Ax is one); a
// drain scenario needs it.
type TaskInspector interface {
	// TaskWorkers maps each task on a worker to that worker's IP; a task
	// with none (suspended, or not yet placed) is left out.
	TaskWorkers(ctx context.Context) (map[string]string, error)
	// TaskConditions are the task's conditions by type.
	TaskConditions(ctx context.Context, task string) (map[string]Condition, error)
}

// WorkerPods deletes the worker pod with an IP (cluster.Kube is one).
type WorkerPods interface {
	// DeleteWorkerPod deletes the pod with podIP ip in the workers'
	// namespace, with its default grace period, and names it.
	DeleteWorkerPod(ctx context.Context, ip string) (string, error)
}

// Options control Run.
type Options struct {
	// Poll is the wait between checks (default 15s).
	Poll time.Duration
	// Keep leaves the task and workload in place for inspection.
	Keep bool
	// Now and Sleep default to the real clock; tests replace them.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Log receives progress lines.
	Log func(format string, args ...any)
	// Archive, when set, stores the run's record once it is decided
	// (#48): the session's full event stream (transcript.sse), its usage
	// (usage.json), the result (result.json) and what ran (meta.json),
	// under <scenario>/<date>/<workload>/. A failed save is logged and
	// doesn't fail the run.
	Archive Archiver
	// Judge, when set, grades a settled answer against the scenario's
	// expect.diagnosis (#90). Its grade goes in the result; neither a low
	// score nor a failed call fails the run.
	Judge Grader
}

// Grader grades an answer against a ground-truth diagnosis (judge.Judge
// is one). It returns a Grade naming the grader even on an error.
type Grader interface {
	Grade(ctx context.Context, expected, answer string) (judge.Grade, error)
}

// judgeTimeout bounds grading: one model call, at most a few minutes at
// high effort.
const judgeTimeout = 3 * time.Minute

// Archiver stores a run's files under dir.
type Archiver interface {
	Save(ctx context.Context, dir string, files map[string][]byte) error
}

// Replayer is an Agent that can hand over a session's raw stream for the
// archive (Router is one).
type Replayer interface {
	Replay(ctx context.Context, task, session string) (*Replay, error)
}

const (
	// collectTimeout bounds reading the session for the archive, apart
	// from the cleanup it runs inside, so it can't take the time the
	// task's deletion needs.
	collectTimeout = 45 * time.Second
	// archiveTimeout bounds saving the archive, the last thing a run
	// does, inside a pod's termination grace period.
	archiveTimeout = time.Minute
)

func (o *Options) defaults() {
	if o.Poll == 0 {
		o.Poll = 15 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// errTimeout marks a wait that ran out.
var errTimeout = errors.New("timed out")

// until polls check every o.Poll until it reports done, fails, or limit
// passes. check gets a context that expires shortly after limit, so a
// slow call can't stretch the wait by much.
func until(ctx context.Context, o *Options, limit time.Duration, check func(context.Context) (bool, error)) error {
	deadline := o.Now().Add(limit)
	cctx, cancel := context.WithTimeout(ctx, limit+time.Minute)
	defer cancel()
	for {
		done, err := check(cctx)
		if err != nil || done {
			return err
		}
		if !o.Now().Before(deadline) {
			return errTimeout
		}
		if err := o.Sleep(ctx, o.Poll); err != nil {
			return err
		}
	}
}

// Leftovers is the label every scenario workload carries, so a run can
// sweep away what a killed run left behind.
const Leftovers = "shipmind.dev/scenario"

// Sweeper deletes a scenario's leftover workloads by label; cluster.Kube
// satisfies it.
type Sweeper interface {
	DeleteLabeled(ctx context.Context, namespace, kinds, selector string) (string, error)
}

// usageTimeout bounds the usage read: the task has just answered, so it
// is awake, and the read is a record, never a reason to wait.
const usageTimeout = 15 * time.Second

// coreAgentDefaultSession is core-agent's own startup session, which no
// incident lands in; reading its stream would cost EventsWait per task.
const coreAgentDefaultSession = "default"

// cleanupTimeout bounds cleanup, which runs even after ctx is cancelled
// (a SIGTERM, a Job's deadline).
const cleanupTimeout = 3 * time.Minute

// Run runs scenario s once and reports what happened. Once it has broken
// the workload, it always returns a Result, whose Failures say what went
// wrong; the error is non-nil only when the run couldn't finish (ctx
// cancelled) or couldn't start. Cleanup (the workload first, so lookout
// stops sending, then the task) runs on every path, on a context that
// survives ctx's cancellation.
func incidentManifest(s *Scenario, name string) ([]byte, error) {
	if s.Chat != nil {
		return nil, fmt.Errorf("%s is a chat scenario; use RunChat", s.Name)
	}
	return s.Manifest(name)
}

func Run(ctx context.Context, s *Scenario, k Kube, tasks Tasks, agent Agent, o Options) (r *Result, err error) {
	o.defaults()
	start := o.Now()
	name := RunName(s.Name, start)
	r = &Result{Scenario: s.Name, Workload: name}
	failf := func(format string, args ...any) { r.Failures = append(r.Failures, fmt.Sprintf(format, args...)) }
	// collected is the session's record, read before cleanup deletes
	// the task, for the archive.
	collected := map[string][]byte{}
	var server string
	var truncated, collectedOnce bool
	collect := func() {
		if o.Archive == nil || r.Task == "" || collectedOnce {
			return
		}
		collectedOnce = true
		if ctx.Err() != nil {
			// Being stopped (a deadline, a SIGTERM): the time left is the
			// task's deletion's; the archive keeps the result alone.
			o.Log("archive: not reading the session: %v", ctx.Err())
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), collectTimeout)
		defer cancel()
		if rp, ok := agent.(Replayer); ok {
			if tr, err := rp.Replay(cctx, r.Task, r.Session); err != nil {
				o.Log("archive: reading the session: %v", err)
			} else {
				collected["transcript.sse"], server, truncated = tr.Raw, tr.Server, tr.Truncated
			}
		}
		uctx, ucancel := context.WithTimeout(cctx, usageTimeout)
		defer ucancel()
		if u, err := agent.Usage(uctx, r.Task, r.Session); err != nil {
			o.Log("archive: reading usage: %v", err)
		} else if b, err := json.MarshalIndent(u, "", "  "); err == nil {
			collected["usage.json"] = b
		}
	}
	// Registered first, so it runs last: cleanup failures count too, and
	// the archive gets the decided result.
	defer func() {
		if r == nil {
			return
		}
		r.Pass = len(r.Failures) == 0 && err == nil
		if o.Archive == nil {
			return
		}
		collect() // with -keep the task is still there
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), archiveTimeout)
		defer cancel()
		files := map[string][]byte{}
		for k, v := range collected {
			files[k] = v
		}
		if b, err := json.MarshalIndent(r, "", "  "); err == nil {
			files["result.json"] = b
		}
		meta := map[string]any{
			"schema": 1, "scenario": s.Name, "workload": name, "namespace": s.Namespace,
			"task": r.Task, "session": r.Session, "agent": server,
			"started_at": start.UTC().Format(time.RFC3339), "finished_at": o.Now().UTC().Format(time.RFC3339),
			"runner": runnerRevision(), "transcript_truncated": truncated,
		}
		if err != nil {
			meta["error"] = err.Error()
		}
		if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
			files["meta.json"] = b
		}
		dir := s.Name + "/" + start.UTC().Format("2006-01-02") + "/" + name
		if serr := o.Archive.Save(actx, dir, files); serr != nil {
			o.Log("archive: %v", serr)
			return
		}
		o.Log("archived %d files under %s", len(files), dir)
	}()

	manifest, err := incidentManifest(s, name)
	if err != nil {
		return nil, err
	}
	seen := sweep(ctx, s, k, tasks, agent, &o)
	before, err := tasks.Tasks(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := k.Apply(ctx, manifest, false); err != nil {
		return nil, fmt.Errorf("breaking %s/%s: %w", s.Namespace, name, err)
	}
	o.Log("broke %s/%s", s.Namespace, name)

	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	}
	workloadGone := false
	removeWorkload := func() {
		if workloadGone || o.Keep {
			return
		}
		cctx, cancel := cleanupCtx()
		defer cancel()
		if _, err := k.Delete(cctx, manifest); err != nil {
			failf("removing the workload: %v", err)
			return
		}
		workloadGone = true
		o.Log("removed %s/%s", s.Namespace, name)
	}
	// Deferred cleanup: the workload first (while it stays broken,
	// lookout keeps sending and would recreate a deleted task), then the
	// task, found late if the incident wait gave up before it appeared.
	defer func() {
		removeWorkload()
		if o.Keep {
			return
		}
		cctx, cancel := cleanupCtx()
		defer cancel()
		if r.Task == "" {
			// Cleanup deletes only a dedicated per-incident task, never a
			// shared storm task; reuse seen so unrelated tasks are not
			// re-probed or woken during cleanup.
			m := findIncident(cctx, tasks, agent, before, s.Namespace, name, seen, false)
			r.Task, r.Session = m.task, m.session
		}
		if r.Task == "" {
			return
		}
		collect()
		if err := tasks.DeleteTask(cctx, r.Task); err != nil {
			failf("deleting task %s: %v", r.Task, err)
			return
		}
		o.Log("deleted task %s", r.Task)
	}()

	// 1. lookout opens an incident: a new lookout-* task whose session
	// names the workload (or folds it into a shared storm task, #132).
	if ok, werr := waitForIncident(ctx, s, tasks, agent, before, name, start, seen, r, &o); !ok || werr != nil {
		return r, werr
	}

	// checkTrajectory grades the path to the answer (#89) and records it.
	checkTrajectory := func(tr *Transcript) {
		tj := s.Expect.Trajectory
		if tj == nil || tr == nil {
			return
		}
		modelCalls := -1
		if r.Usage != nil {
			modelCalls = r.Usage.ModelCalls
		}
		path := &TrajectoryResult{ToolCalls: tr.AnswerCalls(), ModelCalls: max(modelCalls, 0)}
		path.Failures, path.Notes = tj.Check(path.ToolCalls, modelCalls)
		r.Trajectory = path
		for _, f := range path.Failures {
			failf("trajectory: %s", f)
		}
		o.Log("trajectory: %d tool calls (%s); %d failures", len(path.ToolCalls), strings.Join(path.ToolCalls, ", "), len(path.Failures))
	}

	// 2. The agent answers. Settled when idle twice in a row with an
	// answer to the first incident message, or when a later message has
	// arrived after one. An answer still being written doesn't count.
	answeredAt := o.Now()
	idle := 0
	var tr *Transcript
	werr := until(ctx, &o, s.Limits.Diagnose, func(ctx context.Context) (bool, error) {
		state, err := agent.State(ctx, r.Task, r.Session)
		if err != nil {
			return false, nil
		}
		t, err := agent.Events(ctx, r.Task, r.Session)
		if err != nil {
			return false, nil
		}
		tr = t
		answer, superseded := tr.Answer()
		if state == "idle" {
			idle++
		} else {
			idle = 0
		}
		return answer != "" && (superseded || idle >= 2), nil
	})
	if tr != nil {
		r.Answer, r.Superseded = tr.Answer()
		r.CostUSD = tr.Cost
	}
	switch {
	case errors.Is(werr, errTimeout):
		failf("no settled answer within %s", s.Limits.Diagnose)
		checkTrajectory(tr) // a runaway is this case: record what it did
		return r, nil
	case werr != nil:
		return r, werr
	}
	r.AnswerAfter = o.Now().Sub(answeredAt).Round(time.Second).String()
	if r.Missing, err = s.Score(r.Answer, name); err != nil {
		return r, err
	}
	r.Correct = len(r.Missing) == 0
	if !r.Correct {
		failf("the answer lacks %s", strings.Join(r.Missing, ", "))
	}
	o.Log("answered after %s, correct=%v, cost $%.3f", r.AnswerAfter, r.Correct, r.CostUSD)
	uctx, cancel := context.WithTimeout(ctx, usageTimeout)
	u, err := agent.Usage(uctx, r.Task, r.Session)
	cancel()
	if err != nil {
		o.Log("usage: not read: %v", err)
	} else {
		r.Usage = u.Normalize(tr.Server, o.Now())
		cached := 0.0
		if r.Usage.InputTokens > 0 {
			cached = 100 * float64(r.Usage.CachedInputTokens) / float64(r.Usage.InputTokens)
		}
		o.Log("usage: %d model calls, %d input tokens (%.0f%% cached), %d output, %d thinking",
			r.Usage.ModelCalls, r.Usage.InputTokens, cached, r.Usage.OutputTokens, r.Usage.ThinkingTokens)
	}
	checkTrajectory(tr)
	events := len(tr.Events)

	if o.Keep {
		gradeAnswer(ctx, s, name, r, &o)
		return r, nil
	}

	// 3. With the workload gone, lookout sends "resolved" and stops; the
	// task suspends once idle. The judge grades the answer meanwhile: only
	// after the removal, so lookout doesn't re-send the incident (another
	// agent turn, outside the usage already read) while it waits.
	removeWorkload()
	removedAt := o.Now()
	gradeAnswer(ctx, s, name, r, &o)
	if s.Drain != nil {
		// 3'. A worker drain, in place of the idle suspend (#105).
		if ok, err := drainWorker(ctx, s, k, tasks, r, &o); !ok || err != nil {
			return r, err
		}
	} else {
		werr = until(ctx, &o, s.Limits.Suspend, func(ctx context.Context) (bool, error) {
			now, err := tasks.Tasks(ctx)
			if err != nil {
				return false, nil // ax unreachable for a moment: try again
			}
			return now[r.Task] == "Suspended", nil
		})
		switch {
		case errors.Is(werr, errTimeout):
			failf("task %s did not suspend within %s of the workload's removal", r.Task, s.Limits.Suspend)
			return r, nil
		case werr != nil:
			return r, werr
		}
		r.Suspended = true
		r.SuspendAfter = o.Now().Sub(removedAt).Round(time.Second).String()
		o.Log("suspended after %s", r.SuspendAfter)
	}

	// 4. Wake it through the router. The agent lists a session in its
	// memory as "active" (a restarted core-agent lists its stored sessions
	// as "idle"), and this is the first request since the suspend, so an
	// active session with its history means the task resumed from its
	// snapshot rather than restarting.
	sessions, err := agent.Sessions(ctx, r.Task)
	if err != nil {
		failf("waking task %s: %v", r.Task, err)
		return r, nil
	}
	i := slices.IndexFunc(sessions, func(s Session) bool { return s.ID == r.Session })
	switch {
	case i < 0:
		failf("after waking, task %s does not list session %s: the agent restarted instead of resuming", r.Task, r.Session)
	case sessions[i].Status != "active":
		failf("after waking, session %s is %q, not active: the agent restarted instead of resuming", r.Session, sessions[i].Status)
	default:
		woken, err := agent.Events(ctx, r.Task, r.Session)
		if err != nil || len(woken.Events) < events {
			failf("after waking, session %s lost history (%d events, had %d): %v", r.Session, lenEvents(woken), events, err)
			break
		}
		r.Woke = true
		o.Log("woke with the session in memory")
	}
	return r, nil
}

// isRevert reports whether an AutoSuspended reason is one of ax's reverts
// to an earlier snapshot (RevertedAfterWorkerLoss, RevertedAfterFailedSuspend,
// and any later Reverted*): the task survives, the work since its last
// snapshot doesn't.
func isRevert(reason string) bool { return strings.HasPrefix(reason, "Reverted") }

// drainWorker deletes the worker pod hosting the task's actor, as a node
// drain does, and waits for the task, and every other task that was on that
// worker, to suspend for the scenario's reason. It reports whether the run
// should go on to the wake; failures are recorded in r and logged.
func drainWorker(ctx context.Context, s *Scenario, k Kube, tasks Tasks, r *Result, o *Options) (bool, error) {
	failf := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		r.Failures = append(r.Failures, msg)
		o.Log("%s", msg)
	}
	ti, ok1 := tasks.(TaskInspector)
	wp, ok2 := k.(WorkerPods)
	if !ok1 || !ok2 {
		failf("drain: this runner can't find or delete a task's worker")
		return false, nil
	}
	// The task is still awake: the runner's own reads of its session keep
	// it from idling, and lookout's resolved signal comes only after its
	// recovery window.
	var ip string
	var others []string
	werr := until(ctx, o, time.Minute, func(ctx context.Context) (bool, error) {
		workers, err := ti.TaskWorkers(ctx)
		if err != nil {
			return false, nil
		}
		ip, others = workers[r.Task], nil
		for t, w := range workers {
			if t != r.Task && w == ip {
				others = append(others, t)
			}
		}
		return ip != "", nil
	})
	switch {
	case errors.Is(werr, errTimeout):
		failf("drain: task %s isn't on a worker to drain", r.Task)
		return false, nil
	case werr != nil:
		return false, werr
	}
	slices.Sort(others)
	// Deleting the pod drains every actor on it, as a node drain does: the
	// other tasks there (incidents, Slack threads) must come through it too,
	// suspended for the drain and resumed at their next request. The
	// runner sees its own atespace's tasks only.
	pod, err := wp.DeleteWorkerPod(ctx, ip)
	if err != nil {
		failf("drain: deleting the worker pod at %s: %v", ip, err)
		return false, nil
	}
	drainedAt := o.Now()
	r.Drain = &DrainResult{WorkerPod: pod}
	if len(others) > 0 {
		o.Log("drain: deleted worker pod %s (%s), hosting task %s and %s", pod, ip, r.Task, strings.Join(others, ", "))
	} else {
		o.Log("drain: deleted worker pod %s (%s), hosting task %s", pod, ip, r.Task)
	}

	// Each task is settled once ax has written AutoSuspended=True since the
	// deletion for the drain or as a revert, once it has
	// Failed or gone from ax, or, for another task, once a request has
	// resumed it on a different worker. Not the phase at the moment of a
	// poll: lookout's resolved signal, the runner's reads or a person's
	// message can wake a task again between polls, and an AutoSuspended left
	// from an earlier suspend is not this drain's. What a task showed is
	// kept once seen; a phase is read before the condition, so a task seen
	// Failed in the same poll counts as Failed. The scenario's own task must be seen suspended: the
	// wake that follows checks its session, so a resume before a poll saw it
	// suspended doesn't count for it.
	seen := map[string]*drainSeen{r.Task: {}}
	for _, t := range others {
		seen[t] = &drainSeen{}
	}
	poll := func(ctx context.Context) {
		phases, perr := tasks.Tasks(ctx)
		var workers map[string]string
		for t, v := range seen {
			if v.done {
				continue
			}
			if perr == nil {
				p, ok := phases[t]
				if !ok {
					v.gone, v.done = true, true
					continue
				}
				v.phase = p
			}
			if v.phase == "Failed" {
				v.done = true
				continue
			}
			conds, err := ti.TaskConditions(ctx, t)
			if err != nil {
				continue
			}
			if c := conds["AutoSuspended"]; !c.Updated.Before(drainedAt) {
				switch {
				case c.Status == "True" && (c.Reason == s.Drain.Reason || isRevert(c.Reason)):
					v.cond, v.done = c, true
					continue
				case c.Status == "True":
					// Suspended for another reason (idle, at the same
					// moment): its asynchronous snapshot can still fail and
					// end in a revert or Failed, so keep watching it until
					// the window ends.
					v.cond = c
					continue
				case c.Status == "False" && c.Reason == "ResumedByRequest":
					v.resumed = true
				}
			}
			// A resume ax records within its pass interval of the deletion
			// can be one from just before it; still being on the drained
			// worker would mean the drain left the task alone.
			if v.resumed && t != r.Task {
				if workers == nil {
					if workers, err = ti.TaskWorkers(ctx); err != nil {
						continue
					}
				}
				if workers[t] != ip {
					v.movedOff, v.done = true, true
				}
			}
		}
	}
	werr = until(ctx, o, s.Drain.Within, func(ctx context.Context) (bool, error) {
		poll(ctx)
		for _, v := range seen {
			if !v.done {
				return false, nil
			}
		}
		return true, nil
	})
	if werr != nil && !errors.Is(werr, errTimeout) {
		return false, werr
	}
	for _, t := range others {
		outcome, ok := seen[t].outcome(s.Drain.Reason)
		r.Drain.Neighbours = append(r.Drain.Neighbours, NeighbourResult{Task: t, Outcome: outcome})
		if !ok {
			failf("drain: task %s, on the same worker: %s", t, outcome)
		}
	}
	own := seen[r.Task]
	r.Drain.Reason, r.Drain.Message = own.cond.Reason, own.cond.Message
	switch {
	case own.phase == "Failed":
		failf("drain: task %s Failed after its worker's deletion", r.Task)
		return false, nil
	case own.cond.Reason == "":
		failf("drain: task %s did not suspend for the drain within %s of its worker's deletion (phase %s)", r.Task, s.Drain.Within, own.phase)
		return false, nil
	}
	r.Drain.After = own.cond.Updated.Sub(drainedAt).Round(time.Second).String()
	r.Suspended = true
	o.Log("drain: suspended %s after the deletion, reason %s: %s", r.Drain.After, own.cond.Reason, own.cond.Message)
	if own.cond.Reason != s.Drain.Reason {
		// A revert keeps the task but not its session, and another reason
		// isn't this drain's; the wake check below would misreport either.
		failf("drain: task %s suspended with reason %s, want %s", r.Task, own.cond.Reason, s.Drain.Reason)
		return false, nil
	}
	return true, nil
}

// neighbourResumed is a neighbour's outcome when ax shows a request resumed
// it on another worker since the deletion, before the runner saw it
// suspended: the drain suspended it between two polls.
const neighbourResumed = "resumed on another worker before it was seen suspended"

// drainSeen is what the runner has seen of one task since the deletion.
type drainSeen struct {
	done bool
	// cond is the first AutoSuspended=True written since the deletion.
	cond    Condition
	phase   string
	gone    bool
	resumed bool
	// movedOff: resumed, and no longer on the drained worker.
	movedOff bool
}

// outcome says how a task that shared the drained worker came through it,
// and whether that's fine. Its state is safe when it was suspended (for the
// drain, or idle at the same moment), resumed elsewhere, or deleted; not
// when it was reverted to an older snapshot, failed, or left where it was.
func (v *drainSeen) outcome(reason string) (string, bool) {
	switch {
	case v.phase == "Failed":
		return "Failed", false
	case v.gone:
		return "deleted during the drain", true
	case v.cond.Reason == reason:
		return reason, true
	case isRevert(v.cond.Reason):
		return "suspended with reason " + v.cond.Reason + ", want " + reason, false
	case v.cond.Reason != "":
		return "suspended with reason " + v.cond.Reason + " (not the drain's)", true
	case v.movedOff:
		return neighbourResumed, true
	case v.resumed:
		return "resumed but still on the drained worker", false
	default:
		return "no sign of a drain suspend (phase " + v.phase + ")", false
	}
}

// gradeAnswer has the run's judge, if any, grade its settled answer
// against the scenario's diagnosis, and records the grade.
func gradeAnswer(ctx context.Context, s *Scenario, name string, r *Result, o *Options) {
	if o.Judge == nil {
		return
	}
	expected, err := s.Diagnosis(name)
	switch {
	case err != nil:
		o.Log("judge: not grading: expect.diagnosis: %v", err)
		return
	case expected == "":
		o.Log("judge: not grading: the scenario has no expect.diagnosis")
		return
	}
	jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()
	g, err := o.Judge.Grade(jctx, expected, r.Answer)
	r.Judge = &g
	if err != nil {
		o.Log("judge: %v", err)
		return
	}
	o.Log("judge: %s scored %d/5 (correct diagnosis %v, specific %v, actionable %v), $%.3f",
		g.Model, g.Score, g.CorrectDiagnosis, g.Specific, g.Actionable, g.CostUSD)
}

// incidentTask matches the tasks lookout opens per incident:
// "lookout-" and 16 hex digits. Its other tasks (the watchboard,
// "lookout-wb-<cluster>-<hash>", and cluster-scope ones) are long-lived
// agents that also hear about a scenario's workload, in digests and
// resolved signals, and must never be taken for a run's task.
var incidentTask = regexp.MustCompile(`^lookout-[0-9a-f]{16}$`)

// taskClass remembers what kind of lookout-* task a previous inspection
// found so waitForIncident does not re-read unrelated per-incident tasks
// (each Router.Events read holds an SSE stream open for EventsWait and
// resets the task's idle timer) or wake a storm task that has suspended.
type taskClass uint8

const (
	taskUnknown taskClass = iota
	taskUnrelated
	taskStorm
)

// waitForIncident polls for a new dedicated lookout-* task for workload
// in s.Namespace. Under --storm=auto (#132), if lookout folds the workload
// into a shared storm task instead of opening a dedicated task, it records
// r.Storm and a failure and returns (false, nil) so the run removes its
// workload and exits without grading, draining, or deleting the storm task.
func waitForIncident(ctx context.Context, s *Scenario, tasks Tasks, agent Agent, before map[string]string, name string, start time.Time, seen map[string]taskClass, r *Result, o *Options) (bool, error) {
	if seen == nil {
		seen = map[string]taskClass{}
	}
	var lastStorm *StormMatch
	werr := until(ctx, o, s.Limits.Incident, func(ctx context.Context) (bool, error) {
		m := findIncident(ctx, tasks, agent, before, s.Namespace, name, seen, true)
		if m.task != "" {
			r.Task, r.Session = m.task, m.session
			return true, nil
		}
		if m.storm != nil {
			lastStorm = m.storm
			if !m.booting && m.storm.MemberSessionID == "" {
				r.Storm = m.storm
				return true, nil
			}
		}
		return false, nil
	})
	if werr != nil {
		if errors.Is(werr, errTimeout) {
			if lastStorm != nil {
				r.Storm = lastStorm
				o.Log("incident folded into storm task %s (session %s, %s, kind %s) before timeout; shared storm task will not be graded or deleted",
					r.Storm.Task, r.Storm.Session, r.Storm.Ancestor(), r.Storm.Kind)
				r.Failures = append(r.Failures, fmt.Sprintf("workload %s was folded into storm task %s (%s); shared storm tasks are not graded or deleted by scenario runs", name, r.Storm.Task, r.Storm.Ancestor()))
				return false, nil
			}
			r.Failures = append(r.Failures, fmt.Sprintf("no lookout incident task named %s within %s", name, s.Limits.Incident))
			return false, nil
		}
		return false, werr
	}
	r.IncidentAfter = o.Now().Sub(start).Round(time.Second).String()
	if r.Storm != nil {
		o.Log("incident folded into storm task %s (session %s, %s, kind %s) after %s; shared storm task will not be graded or deleted",
			r.Storm.Task, r.Storm.Session, r.Storm.Ancestor(), r.Storm.Kind, r.IncidentAfter)
		r.Failures = append(r.Failures, fmt.Sprintf("workload %s was folded into storm task %s (%s); shared storm tasks are not graded or deleted by scenario runs", name, r.Storm.Task, r.Storm.Ancestor()))
		return false, nil
	}
	o.Log("incident: task %s, session %s, after %s", r.Task, r.Session, r.IncidentAfter)
	return true, nil
}

// incidentLookup is one poll's outcome from findIncident: either a
// dedicated per-incident task (task != "") or a shared storm task that
// folded the workload (storm != nil). booting is true when a brand-new
// lookout task has not yet exposed a session with its first message.
type incidentLookup struct {
	task    string
	session string
	storm   *StormMatch
	booting bool
}

// findIncident looks for the new dedicated incident task (one not in
// before) whose first message is about workload in ns. When checkStorms
// is true and no dedicated task matches yet, it also inspects new tasks
// and already-Running pre-existing lookout-* tasks (skipping Failed tasks,
// tasks already classified as unrelated in seen, and any Suspended storm
// or pre-existing task so it never wakes them) to detect whether lookout
// folded the workload into a shared storm task.
func findIncident(ctx context.Context, tasks Tasks, agent Agent, before map[string]string, ns, workload string, seen map[string]taskClass, checkStorms bool) incidentLookup {
	now, err := tasks.Tasks(ctx)
	if err != nil {
		return incidentLookup{}
	}
	var newNames, runningOld []string
	for t, phase := range now {
		if !incidentTask.MatchString(t) || phase == "Failed" || seen[t] == taskUnrelated {
			continue
		}
		if _, old := before[t]; !old {
			if seen[t] == taskStorm && phase != "Running" {
				continue
			}
			newNames = append(newNames, t)
		} else if checkStorms && phase == "Running" {
			runningOld = append(runningOld, t)
		}
	}
	slices.Sort(newNames)
	var out incidentLookup
	for _, t := range newNames {
		sessions, err := agent.Sessions(ctx, t)
		if err != nil {
			out.booting = true
			continue // still booting
		}
		inspected, incomplete, isStorm := false, false, false
		for _, sess := range sessions {
			if sess.ID == coreAgentDefaultSession {
				continue
			}
			tr, err := agent.Events(ctx, t, sess.ID)
			if err != nil || tr == nil || tr.FirstMessage() == "" {
				out.booting, incomplete = true, true
				continue
			}
			inspected = true
			if in, ok := tr.FirstIncident(); ok && in.About(ns, workload) {
				return incidentLookup{task: t, session: sess.ID}
			}
			if tr.IsStorm() {
				isStorm = true
				if checkStorms && out.storm == nil {
					if sm, ok := tr.StormContaining(ns, workload); ok {
						sm.Task, sm.Session = t, sess.ID
						out.storm = &sm
					}
				}
			}
		}
		if !inspected {
			out.booting = true
		} else if seen != nil && !incomplete {
			if isStorm {
				seen[t] = taskStorm
			} else {
				seen[t] = taskUnrelated
			}
		}
	}
	if !checkStorms || out.storm != nil {
		return out
	}
	slices.Sort(runningOld)
	for _, t := range runningOld {
		if sm := findStormInTask(ctx, agent, t, ns, workload, seen); sm != nil {
			out.storm = sm
			return out
		}
	}
	return out
}

func findStormInTask(ctx context.Context, agent Agent, task, ns, workload string, seen map[string]taskClass) *StormMatch {
	sessions, err := agent.Sessions(ctx, task)
	if err != nil {
		return nil
	}
	inspected, incomplete, isStorm := false, false, false
	for _, sess := range sessions {
		if sess.ID == coreAgentDefaultSession {
			continue
		}
		tr, err := agent.Events(ctx, task, sess.ID)
		if err != nil || tr == nil || tr.FirstMessage() == "" {
			incomplete = true
			continue
		}
		inspected = true
		if tr.IsStorm() {
			isStorm = true
			if sm, ok := tr.StormContaining(ns, workload); ok {
				if seen != nil {
					seen[task] = taskStorm
				}
				sm.Task, sm.Session = task, sess.ID
				return &sm
			}
		}
	}
	if seen != nil && inspected && !incomplete {
		if isStorm {
			seen[task] = taskStorm
		} else {
			seen[task] = taskUnrelated
		}
	}
	return nil
}

// sweep removes what killed runs of this scenario left behind: its
// labeled workloads, and incident tasks whose incident (their first
// message, parsed) is about one of its run workloads in its namespace.
// Nothing else: lookout's watchboard, cluster-scope and storm tasks hear
// about scenario workloads too, and are long-lived agents. Best effort:
// failures are logged, not fatal. It returns the classifications of the
// tasks it already inspected and kept so waitForIncident skips unrelated
// non-storm tasks on later polls.
func sweep(ctx context.Context, s *Scenario, k Kube, tasks Tasks, agent Agent, o *Options) map[string]taskClass {
	seen := map[string]taskClass{}
	if o.Keep {
		return seen
	}
	if sw, ok := k.(Sweeper); ok {
		if out, err := sw.DeleteLabeled(ctx, s.Namespace, "deployments,configmaps", Leftovers+"="+s.Name); err != nil {
			o.Log("sweep: %v", err)
		} else if out = strings.TrimSpace(out); out != "" && !strings.HasPrefix(out, "No resources found") {
			o.Log("sweep: %s", out)
		}
	}
	now, err := tasks.Tasks(ctx)
	if err != nil {
		o.Log("sweep: %v", err)
		return seen
	}
	prefix := s.Name + "-"
	for t := range now {
		if !incidentTask.MatchString(t) {
			continue
		}
		sessions, err := agent.Sessions(ctx, t)
		if err != nil {
			continue
		}
		inspected, incomplete, isStorm, deleted := false, false, false, false
		for _, sess := range sessions {
			if sess.ID == coreAgentDefaultSession {
				continue
			}
			tr, err := agent.Events(ctx, t, sess.ID)
			if err != nil || tr == nil || tr.FirstMessage() == "" {
				incomplete = true
				continue
			}
			inspected = true
			if tr.IsStorm() {
				isStorm = true
				continue
			}
			in, ok := tr.FirstIncident()
			if !ok || in.Namespace != s.Namespace || !strings.HasPrefix(in.Name, prefix) || !mentionsRun(in.Name, prefix) {
				continue
			}
			if err := tasks.DeleteTask(ctx, t); err != nil {
				o.Log("sweep: deleting task %s: %v", t, err)
			} else {
				o.Log("sweep: deleted task %s, left by an earlier run", t)
			}
			deleted = true
			break
		}
		if !deleted && inspected && !incomplete {
			if isStorm {
				seen[t] = taskStorm
			} else {
				seen[t] = taskUnrelated
			}
		}
	}
	return seen
}

func lenEvents(t *Transcript) int {
	if t == nil {
		return 0
	}
	return len(t.Events)
}

// runnerRevision is the shipmind commit the runner was built from, when
// the build recorded it.
func runnerRevision() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			if kv.Key == "vcs.revision" {
				return kv.Value
			}
		}
	}
	return ""
}
