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
	"errors"
	"fmt"
	"os/exec"
	"regexp"
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

	Correct bool `json:"correct"`
	// Suspended: the task suspended once the workload was gone.
	Suspended bool `json:"suspended"`
	// Woke: the session answered again through the router, still in
	// mast's memory (status "active") with its history intact.
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
}

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
	// Registered first, so it runs last: cleanup failures count too.
	defer func() {
		if r != nil {
			r.Pass = len(r.Failures) == 0 && err == nil
		}
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
		r.Answer, _ = tr.Answer()
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

	// 4. Wake it through the router. mast lists only the sessions in its
	// memory (a restarted mast lists none until one is used), and this is
	// the first request since the suspend, so a listed session with its
	// history means the task resumed from its snapshot rather than
	// restarting.
	sessions, err := agent.Sessions(ctx, r.Task)
	if err != nil {
		failf("waking task %s: %v", r.Task, err)
		return r, nil
	}
	i := slices.IndexFunc(sessions, func(s Session) bool { return s.ID == r.Session })
	switch {
	case i < 0:
		failf("after waking, task %s does not list session %s: mast restarted instead of resuming", r.Task, r.Session)
	case sessions[i].Status != "active":
		failf("after waking, session %s is %q, not active: mast restarted instead of resuming", r.Session, sessions[i].Status)
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
