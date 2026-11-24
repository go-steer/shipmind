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
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// ChatTasks starts and removes what a chat run runs on (Ax is one).
type ChatTasks interface {
	// Apply applies one ax manifest (a Task or a Workspace).
	Apply(ctx context.Context, manifest []byte) error
	DeleteTask(ctx context.Context, name string) error
	DeleteWorkspace(ctx context.Context, name string) error
}

// ChatAgent is an Agent a run can also talk to (Router is one).
type ChatAgent interface {
	Agent
	CreateSession(ctx context.Context, task string) (string, error)
	Inject(ctx context.Context, task, session, message string) error
	SubagentEvents(ctx context.Context, task, session, name string) (t *Transcript, known bool, err error)
}

// FixtureReadier is an optional Kube capability (cluster.Kube satisfies it)
// used by seeded chat scenarios to wait until the planted Deployment's pods
// are Running before waking the agent (leeway scores Running pods only).
type FixtureReadier interface {
	PodStarts(ctx context.Context, namespace, deployment string) ([]time.Time, error)
}

// ChatRun is what a chat run is handed: the agent to start and the
// cluster to ask it about. The manifests are rendered by whoever
// launches the run (`shipmind scenario canary`), from an environment and
// a candidate agent image, so the run exercises that image in the task
// shape the candidate checkout renders, without the environment's own
// chat task, Workspace or switchboard changing (#127).
type ChatRun struct {
	// Task is the ax Task manifest, named.
	Task []byte
	// Workspace, if set, is the ax Workspace the task mounts, named;
	// the run applies it first and deletes it last.
	Workspace []byte
	// Cluster is the cluster's full path,
	// projects/<project>/locations/<location>/clusters/<name>.
	Cluster string
	// Kube, when s.Workload is set, plants the scenario's seeded fixture
	// before asking and removes it during cleanup.
	Kube Kube
}

var chatCluster = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/locations/[a-z0-9-]+/clusters/[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// manifestName returns the metadata.name of an ax manifest of kind.
func manifestName(manifest []byte, kind string) (string, error) {
	var m struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(manifest, &m); err != nil {
		return "", fmt.Errorf("the %s manifest: %w", kind, err)
	}
	if m.Kind != kind || !nameRE.MatchString(m.Metadata.Name) {
		return "", fmt.Errorf("the %s manifest: want kind %s with a metadata.name, got kind %q name %q", kind, kind, m.Kind, m.Metadata.Name)
	}
	return m.Metadata.Name, nil
}

// Apply implements ChatTasks with `ax apply -f -`.
func (a Ax) Apply(ctx context.Context, manifest []byte) error {
	_, err := a.runStdin(ctx, manifest, "apply", "-f", "-")
	return err
}

// DeleteWorkspace implements ChatTasks.
func (a Ax) DeleteWorkspace(ctx context.Context, name string) error {
	_, err := a.run(ctx, "delete", "workspace", name)
	return err
}

// chat is one chat run in progress.
type chat struct {
	s     *Scenario
	tasks ChatTasks
	agent ChatAgent
	o     *Options
	r     *Result

	// task and workspace are what the run tried to apply; a task that
	// applied is also in r.Task.
	name, task, workspace string
	workspaceApplied      bool
	kube                  Kube
	fixture               []byte
	fixtureApplied        bool
	start                 time.Time
	// collected is the session's record, read before cleanup deletes the
	// task, for the archive.
	collected map[string][]byte
	server    string
	truncated bool
	read      bool
}

func (c *chat) failf(format string, args ...any) {
	c.r.Failures = append(c.r.Failures, fmt.Sprintf(format, args...))
}

func fixtureDesiredReplicas(manifest []byte) int {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Replicas *int `yaml:"replicas"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			return 1
		}
		if doc.Kind == "Deployment" && doc.Spec.Replicas != nil && *doc.Spec.Replicas > 0 {
			return *doc.Spec.Replicas
		}
	}
}

func (c *chat) waitFixtureReady(ctx context.Context) (bool, error) {
	fr, ok := c.kube.(FixtureReadier)
	if !ok {
		return true, nil
	}
	want := fixtureDesiredReplicas(c.fixture)
	var last int
	werr := until(ctx, c.o, c.s.Limits.Start, func(ctx context.Context) (bool, error) {
		starts, err := fr.PodStarts(ctx, c.s.Namespace, c.name)
		if err != nil {
			return false, nil
		}
		last = len(starts)
		return last >= want, nil
	})
	if errors.Is(werr, errTimeout) {
		c.failf("seeded fixture %s/%s reached %d/%d running pods within %s", c.s.Namespace, c.name, last, want, c.s.Limits.Start)
		return false, nil
	}
	if werr != nil {
		return false, werr
	}
	c.o.Log("fixture %s/%s running (%d/%d pods)", c.s.Namespace, c.name, last, want)
	return true, nil
}

// RunChat runs chat scenario s once: it applies the run's Workspace and
// Task, opens a session, sends the scenario's question about run.Cluster,
// waits for a settled answer and grades it. Like Run, once it has started
// anything it returns a Result whose Failures say what went wrong, and an
// error only when the run couldn't start or was cancelled; cleanup (the
// task, then the Workspace) runs on every path.
//
// The answer is graded on the scenario's mentions and absences, on the
// tools called by the main agent and by the scenario's specialists
// together (expect.trajectory), and on those calls' results (see
// trajectory), since a chat run exists to show that the agent's tools
// reach the cluster.
func RunChat(ctx context.Context, s *Scenario, tasks ChatTasks, agent ChatAgent, run ChatRun, o Options) (r *Result, err error) {
	o.defaults()
	if s.Chat == nil {
		return nil, fmt.Errorf("%s is not a chat scenario", s.Name)
	}
	if !chatCluster.MatchString(run.Cluster) {
		return nil, fmt.Errorf("cluster %q: want projects/<project>/locations/<location>/clusters/<name>", run.Cluster)
	}
	task, err := manifestName(run.Task, "Task")
	if err != nil {
		return nil, err
	}
	c := &chat{s: s.ForCluster(run.Cluster), tasks: tasks, agent: agent, o: &o, start: o.Now(), collected: map[string][]byte{}}
	if len(run.Workspace) > 0 {
		if c.workspace, err = manifestName(run.Workspace, "Workspace"); err != nil {
			return nil, err
		}
	}
	c.name = RunName(s.Name, c.start)
	if c.s.Workload != "" {
		if run.Kube == nil {
			return nil, fmt.Errorf("%s plants workload %s: ChatRun.Kube is required", s.Name, c.s.Workload)
		}
		c.kube = run.Kube
		if c.fixture, err = c.s.Manifest(c.name); err != nil {
			return nil, err
		}
	}
	ask, err := c.s.Ask(c.name)
	if err != nil {
		return nil, err
	}
	c.r = &Result{Scenario: s.Name, Workload: c.name, Cluster: run.Cluster}
	r = c.r
	// Registered first, so it runs last: cleanup failures count too.
	defer func() {
		if r == nil {
			return
		}
		r.Pass = len(r.Failures) == 0 && err == nil
		c.archive(ctx, err)
	}()

	// Cleanup is registered before anything is applied: an apply that
	// fails may still have created its object.
	defer c.cleanup(ctx)
	if len(c.fixture) > 0 {
		if _, err := c.kube.Apply(ctx, c.fixture, false); err != nil {
			c.failf("applying seeded fixture %s/%s: %v", c.s.Namespace, c.name, err)
			return r, nil
		}
		c.fixtureApplied = true
		o.Log("planted fixture %s/%s", c.s.Namespace, c.name)
		if ok, err := c.waitFixtureReady(ctx); !ok || err != nil {
			return r, err
		}
	}
	if c.workspace != "" {
		if err := tasks.Apply(ctx, run.Workspace); err != nil {
			c.failf("applying workspace %s: %v", c.workspace, err)
			return r, nil
		}
		c.workspaceApplied = true
		o.Log("applied workspace %s", c.workspace)
	}
	c.task = task
	if err := tasks.Apply(ctx, run.Task); err != nil {
		c.failf("applying task %s: %v", task, err)
		return r, nil
	}
	r.Task = task
	o.Log("applied task %s", task)

	if ok, err := c.open(ctx, ask); !ok || err != nil {
		return r, err
	}
	tr, ok, err := c.settle(ctx)
	if !ok || err != nil {
		return r, err
	}
	if err := c.grade(ctx, tr); err != nil {
		return r, err
	}
	gradeAnswer(ctx, c.s, c.name, r, &o)
	return r, nil
}

// open waits for the task's agent to accept a session, then sends ask.
func (c *chat) open(ctx context.Context, ask string) (bool, error) {
	var last error
	werr := until(ctx, c.o, c.s.Limits.Start, func(ctx context.Context) (bool, error) {
		c.r.Session, last = c.agent.CreateSession(ctx, c.r.Task)
		return last == nil, nil
	})
	switch {
	case errors.Is(werr, errTimeout):
		c.failf("task %s accepted no session within %s: %v", c.r.Task, c.s.Limits.Start, last)
		return false, nil
	case werr != nil:
		return false, werr
	}
	c.r.IncidentAfter = c.o.Now().Sub(c.start).Round(time.Second).String()
	c.o.Log("session %s on task %s, after %s", c.r.Session, c.r.Task, c.r.IncidentAfter)
	if err := c.agent.Inject(ctx, c.r.Task, c.r.Session, ask); err != nil {
		c.failf("sending the question: %v", err)
		return false, nil
	}
	return true, nil
}

// settle waits for the answer: the session idle twice in a row with an
// answer to the question. An answer still being written doesn't count.
func (c *chat) settle(ctx context.Context) (*Transcript, bool, error) {
	askedAt := c.o.Now()
	idle := 0
	var tr *Transcript
	werr := until(ctx, c.o, c.s.Limits.Diagnose, func(ctx context.Context) (bool, error) {
		state, err := c.agent.State(ctx, c.r.Task, c.r.Session)
		if err != nil {
			return false, nil
		}
		t, err := c.agent.Events(ctx, c.r.Task, c.r.Session)
		if err != nil {
			return false, nil
		}
		tr = t
		if state == "idle" {
			idle++
		} else {
			idle = 0
		}
		answer, _ := tr.Answer()
		return answer != "" && idle >= 2, nil
	})
	if tr != nil {
		c.r.Answer, _ = tr.Answer()
		c.r.CostUSD = tr.Cost
	}
	switch {
	case errors.Is(werr, errTimeout):
		c.failf("no settled answer within %s", c.s.Limits.Diagnose)
		if tr != nil {
			c.trajectory(ctx, tr) // a runaway, or a call parked for approval: record what it did
		}
		return nil, false, nil
	case werr != nil:
		return nil, false, werr
	}
	c.r.AnswerAfter = c.o.Now().Sub(askedAt).Round(time.Second).String()
	return tr, true, nil
}

// grade scores the settled answer and the path to it.
func (c *chat) grade(ctx context.Context, tr *Transcript) error {
	var err error
	if c.r.Missing, err = c.s.Score(c.r.Answer, c.name); err != nil {
		return err
	}
	if c.r.Unwanted, err = c.s.Unwanted(c.r.Answer, c.name); err != nil {
		return err
	}
	c.r.Correct = len(c.r.Missing) == 0 && len(c.r.Unwanted) == 0
	if len(c.r.Missing) > 0 {
		c.failf("the answer lacks %s", strings.Join(c.r.Missing, ", "))
	}
	if len(c.r.Unwanted) > 0 {
		c.failf("the answer says %s", strings.Join(c.r.Unwanted, ", "))
	}
	c.o.Log("answered after %s, correct=%v, cost $%.3f", c.r.AnswerAfter, c.r.Correct, c.r.CostUSD)
	uctx, cancel := context.WithTimeout(ctx, usageTimeout)
	u, uerr := c.agent.Usage(uctx, c.r.Task, c.r.Session)
	cancel()
	if uerr != nil {
		c.o.Log("usage: not read: %v", uerr)
	} else {
		c.r.Usage = u.Normalize(tr.Server, c.o.Now())
	}
	c.trajectory(ctx, tr)
	return nil
}

// trajectory records the tools the main agent and the scenario's
// specialists called and what those calls returned, and checks them:
// against expect.trajectory, against expect.results, and for tools that
// failed. A tool whose call failed and that never returned a result
// fails the run; one that failed and then succeeded (a model's slip,
// retried) is noted.
func (c *chat) trajectory(ctx context.Context, tr *Transcript) {
	path := &TrajectoryResult{ToolCalls: tr.AnswerCalls(), ToolErrors: tr.Failures()}
	results := tr.Results()
	for _, sp := range c.s.Chat.Specialists {
		st, known, err := c.agent.SubagentEvents(ctx, c.r.Task, c.r.Session, sp)
		switch {
		case err != nil:
			path.Notes = append(path.Notes, fmt.Sprintf("specialist %s: its calls were not read: %v", sp, err))
			continue
		case !known:
			path.Notes = append(path.Notes, fmt.Sprintf("the agent has no specialist %s", sp))
			continue
		case len(st.Events) == 0:
			path.Notes = append(path.Notes, fmt.Sprintf("specialist %s did not run", sp))
		case st.Truncated:
			path.Notes = append(path.Notes, fmt.Sprintf("specialist %s: its history ran past what was read; its last calls may be missing", sp))
		}
		path.ToolCalls = append(path.ToolCalls, st.Calls()...)
		path.ToolErrors = append(path.ToolErrors, st.Failures()...)
		results = append(results, st.Results()...)
		if c.o.Archive != nil {
			if b, err := json.MarshalIndent(st.Events, "", "  "); err == nil {
				c.collected["specialist-"+sp+".json"] = b
			}
		}
	}
	modelCalls := -1
	if c.r.Usage != nil {
		modelCalls = c.r.Usage.ModelCalls
	}
	path.ModelCalls = max(modelCalls, 0)
	if tj := c.s.Expect.Trajectory; tj != nil {
		failures, notes := tj.Check(path.ToolCalls, modelCalls)
		path.Failures = failures
		path.Notes = append(path.Notes, notes...)
	}
	for _, e := range path.ToolErrors {
		tool, _, _ := strings.Cut(e, ": ")
		if slices.ContainsFunc(results, func(r ToolResult) bool { return r.Tool == tool }) {
			path.Notes = append(path.Notes, "a tool call failed and a later one succeeded: "+e)
			continue
		}
		path.Failures = append(path.Failures, "a tool call failed: "+e)
	}
	path.Failures = append(path.Failures, c.s.CheckResults(results)...)
	c.r.Trajectory = path
	for _, f := range path.Failures {
		c.failf("trajectory: %s", f)
	}
	c.o.Log("trajectory: %d tool calls (%s); %d tool errors; %d failures",
		len(path.ToolCalls), strings.Join(path.ToolCalls, ", "), len(path.ToolErrors), len(path.Failures))
}

// collect reads the session's record for the archive, once, while the
// task still exists.
func (c *chat) collect(ctx context.Context) {
	if c.o.Archive == nil || c.r.Task == "" || c.r.Session == "" || c.read {
		return
	}
	c.read = true
	if ctx.Err() != nil {
		c.o.Log("archive: not reading the session: %v", ctx.Err())
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), collectTimeout)
	defer cancel()
	if rp, ok := c.agent.(Replayer); ok {
		if tr, err := rp.Replay(cctx, c.r.Task, c.r.Session); err != nil {
			c.o.Log("archive: reading the session: %v", err)
		} else {
			c.collected["transcript.sse"], c.server, c.truncated = tr.Raw, tr.Server, tr.Truncated
		}
	}
	uctx, ucancel := context.WithTimeout(cctx, usageTimeout)
	defer ucancel()
	if u, err := c.agent.Usage(uctx, c.r.Task, c.r.Session); err != nil {
		c.o.Log("archive: reading usage: %v", err)
	} else if b, err := json.MarshalIndent(u, "", "  "); err == nil {
		c.collected["usage.json"] = b
	}
}

// cleanup deletes the task, then the Workspace it mounted, each on a
// context of its own that survives the run's cancellation. An apply that
// failed may still have created its object (a timeout after the server
// acted), so whatever the run tried to apply it tries to delete; only a
// delete of something known to exist counts as a failure.
func (c *chat) cleanup(ctx context.Context) {
	if c.o.Keep {
		return
	}
	remove := func(what, name string, exists bool, del func(context.Context, string) error) {
		if name == "" {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		switch err := del(cctx, name); {
		case err == nil:
			c.o.Log("deleted %s %s", what, name)
		case exists:
			c.failf("deleting %s %s: %v", what, name, err)
		default:
			c.o.Log("deleting %s %s, whose apply failed: %v", what, name, err)
		}
	}
	if c.r.Task != "" {
		c.collect(ctx)
	}
	remove("task", c.task, c.r.Task != "", c.tasks.DeleteTask)
	remove("workspace", c.workspace, c.workspaceApplied, c.tasks.DeleteWorkspace)
	if len(c.fixture) > 0 && c.kube != nil {
		remove("fixture", c.s.Namespace+"/"+c.name, c.fixtureApplied, func(cctx context.Context, _ string) error {
			_, err := c.kube.Delete(cctx, c.fixture)
			return err
		})
	}
}

// archive stores the decided run, as Run does.
func (c *chat) archive(ctx context.Context, runErr error) {
	if c.o.Archive == nil {
		return
	}
	c.collect(ctx) // with -keep the task is still there
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), archiveTimeout)
	defer cancel()
	files := map[string][]byte{}
	for k, v := range c.collected {
		files[k] = v
	}
	if b, err := json.MarshalIndent(c.r, "", "  "); err == nil {
		files["result.json"] = b
	}
	meta := map[string]any{
		"schema": 1, "scenario": c.s.Name, "workload": c.name, "kind": "chat", "cluster": c.r.Cluster,
		"task": c.r.Task, "session": c.r.Session, "agent": c.server,
		"started_at": c.start.UTC().Format(time.RFC3339), "finished_at": c.o.Now().UTC().Format(time.RFC3339),
		"runner": runnerRevision(), "transcript_truncated": c.truncated,
	}
	if runErr != nil {
		meta["error"] = runErr.Error()
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		files["meta.json"] = b
	}
	dir := c.s.Name + "/" + c.start.UTC().Format("2006-01-02") + "/" + c.name
	if err := c.o.Archive.Save(actx, dir, files); err != nil {
		c.o.Log("archive: %v", err)
		return
	}
	c.o.Log("archived %d files under %s", len(files), dir)
}

// runStdin is run with input on the command's stdin.
func (a Ax) runStdin(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd, err := a.command(ctx, args...)
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(stdin), &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ax %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
