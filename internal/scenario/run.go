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

func (a Ax) run(ctx context.Context, args ...string) (string, error) {
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
		return "", errors.New("ax: neither a server nor a kube context")
	}
	cmd := exec.CommandContext(ctx, bin, append(append(conn, "-a", a.Atespace), args...)...) //nolint:gosec // fixed binary and arguments
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
	CostUSD float64  `json:"cost_usd"`
	// Superseded: a later message (lookout re-sending the incident)
	// arrived before the answer settled, so Usage may include calls
	// made for it.
	Superseded bool `json:"superseded,omitempty"`
	// Usage is the session's model usage once the answer settled, in
	// one meaning whichever agent answered: model calls, tokens and the
	// cached share, cost per call. Absent when the read failed, which
	// doesn't fail the run.
	Usage *Usage `json:"usage,omitempty"`

	Correct bool `json:"correct"`
	// Suspended: the task suspended once the workload was gone.
	Suspended bool `json:"suspended"`
	// Woke: the session answered again through the router, still in
	// the agent's memory (status "active") with its history intact.
	Woke bool `json:"woke"`

	Pass     bool     `json:"pass"`
	Failures []string `json:"failures,omitempty"`
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
}

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

	manifest, err := s.Manifest(name)
	if err != nil {
		return nil, err
	}
	sweep(ctx, s, k, tasks, agent, &o)
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
			r.Task, r.Session = findIncident(cctx, tasks, agent, before, s.Namespace, name)
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
	// names the workload.
	werr := until(ctx, &o, s.Limits.Incident, func(ctx context.Context) (bool, error) {
		r.Task, r.Session = findIncident(ctx, tasks, agent, before, s.Namespace, name)
		return r.Task != "", nil
	})
	if werr != nil {
		if errors.Is(werr, errTimeout) {
			failf("no lookout incident task named %s within %s", name, s.Limits.Incident)
			return r, nil
		}
		return r, werr
	}
	r.IncidentAfter = o.Now().Sub(start).Round(time.Second).String()
	o.Log("incident: task %s, session %s, after %s", r.Task, r.Session, r.IncidentAfter)

	// 2. The agent answers. Settled when idle twice in a row with an
	// answer to the first incident message, or when a later message has
	// arrived after one. An answer still being written doesn't count.
	answeredAt := o.Now()
	idle := 0
	var tr *Transcript
	werr = until(ctx, &o, s.Limits.Diagnose, func(ctx context.Context) (bool, error) {
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
	events := len(tr.Events)

	if o.Keep {
		return r, nil
	}

	// 3. With the workload gone, lookout sends "resolved" and stops; the
	// task suspends once idle.
	removeWorkload()
	removedAt := o.Now()
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

	// 4. Wake it through the router. The agent lists a session in its
	// memory as "active" (a restarted mast lists none until one is used;
	// a restarted core-agent lists its stored sessions as "idle"), and
	// this is the first request since the suspend, so an active session
	// with its history means the task resumed from its snapshot rather
	// than restarting.
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

// incidentTask matches the tasks lookout opens per incident:
// "lookout-" and 16 hex digits. Its other tasks (the watchboard,
// "lookout-wb-<cluster>-<hash>", and cluster-scope ones) are long-lived
// agents that also hear about a scenario's workload, in digests and
// resolved signals, and must never be taken for a run's task.
var incidentTask = regexp.MustCompile(`^lookout-[0-9a-f]{16}$`)

// findIncident returns the new incident task (one not in before) whose
// incident, its first message, is about the workload, if there is one
// yet.
func findIncident(ctx context.Context, tasks Tasks, agent Agent, before map[string]string, ns, workload string) (task, session string) {
	now, err := tasks.Tasks(ctx)
	if err != nil {
		return "", ""
	}
	names := make([]string, 0, len(now))
	for t := range now {
		if _, old := before[t]; !old && incidentTask.MatchString(t) {
			names = append(names, t)
		}
	}
	slices.Sort(names)
	for _, t := range names {
		sessions, err := agent.Sessions(ctx, t)
		if err != nil {
			continue // still booting
		}
		for _, sess := range sessions {
			if sess.ID == coreAgentDefaultSession {
				continue
			}
			tr, err := agent.Events(ctx, t, sess.ID)
			if err != nil {
				continue
			}
			if in, ok := tr.FirstIncident(); ok && in.About(ns, workload) {
				return t, sess.ID
			}
		}
	}
	return "", ""
}

// sweep removes what killed runs of this scenario left behind: its
// labeled workloads, and incident tasks whose incident (their first
// message, parsed) is about one of its run workloads in its namespace.
// Nothing else: lookout's watchboard, cluster-scope and storm tasks hear
// about scenario workloads too, and are long-lived agents. Best effort:
// failures are logged, not fatal.
func sweep(ctx context.Context, s *Scenario, k Kube, tasks Tasks, agent Agent, o *Options) {
	if o.Keep {
		return
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
		return
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
		for _, sess := range sessions {
			if sess.ID == coreAgentDefaultSession {
				continue
			}
			tr, err := agent.Events(ctx, t, sess.ID)
			if err != nil {
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
			break
		}
	}
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
