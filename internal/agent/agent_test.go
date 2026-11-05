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

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	yaml11 "go.yaml.in/yaml/v2"
	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/env"
)

const root = "../.."

var tok = Tokens{Inject: strings.Repeat("a", 32), Attach: strings.Repeat("b", 32)}

func testEnv() *env.Env {
	return &env.Env{
		Context: "demo",
		Agent: env.Agent{
			Atespace:  "agents",
			Project:   "example-project",
			Workspace: "demo-ws",
			Image:     "registry.example.com/agent@sha256:" + strings.Repeat("0", 64),
			Clusters: []env.Cluster{
				{Name: "demo", Path: "projects/example-project/locations/us-central1/clusters/demo"},
				{Name: "prod", Path: "projects/example-project/locations/europe-west1/clusters/prod"},
			},
		},
	}
}

func TestTask(t *testing.T) {
	b, err := Task(root, testEnv(), tok)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	var task struct {
		Metadata struct{ Atespace string } `yaml:"metadata"`
		Spec     struct {
			Image     string
			Command   []string
			Env       []struct{ Name, Value string }
			Workspace struct{ Name, Path string }
			Resources struct {
				Limits struct{ CPU, Memory string } `yaml:"limits"`
			} `yaml:"resources"`
			HTTP struct{ Port int } `yaml:"http"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &task); err != nil {
		t.Fatalf("rendered task is not YAML: %v\n%s", err, b)
	}
	envs := map[string]string{}
	for _, e := range task.Spec.Env {
		envs[e.Name] = e.Value
	}
	switch {
	case task.Metadata.Atespace != "agents":
		t.Errorf("atespace %q", task.Metadata.Atespace)
	case task.Spec.Image != testEnv().Agent.Image:
		t.Errorf("image %q", task.Spec.Image)
	case envs["GOOGLE_CLOUD_PROJECT"] != "example-project":
		t.Errorf("project %q", envs["GOOGLE_CLOUD_PROJECT"])
	case envs["MAST_INJECT_TOKEN"] != tok.Inject || envs["MAST_ATTACH_TOKEN"] != tok.Attach:
		t.Errorf("tokens %v", envs)
	case task.Spec.Workspace.Name != "demo-ws" || task.Spec.Workspace.Path != "/workspace/repo":
		t.Errorf("workspace %+v", task.Spec.Workspace)
	case task.Spec.Resources.Limits.CPU == "" || task.Spec.Resources.Limits.Memory == "":
		// Substrate v1.0 refuses an actor without both.
		t.Errorf("resources %+v", task.Spec.Resources)
	case task.Spec.HTTP.Port != 8484:
		t.Errorf("http.port %d", task.Spec.HTTP.Port)
	}

	if _, err := Task(root, testEnv(), Tokens{Inject: "x", Attach: tok.Attach}); err == nil {
		t.Error("Task with a bad token succeeded")
	}
}

func TestAgentsMD(t *testing.T) {
	b, err := AgentsMD(root, testEnv())
	if err != nil {
		t.Fatalf("AgentsMD: %v", err)
	}
	want := "| cluster | full name |\n|---|---|\n" +
		"| demo | projects/example-project/locations/us-central1/clusters/demo |\n" +
		"| prod | projects/example-project/locations/europe-west1/clusters/prod |\n" +
		"\nIf the cluster is neither"
	if !strings.Contains(string(b), want) {
		t.Errorf("AGENTS.md lacks the cluster table:\n%s", b)
	}
	if !strings.HasPrefix(string(b), "# Clusters\n") || !strings.Contains(string(b), "# Repeated and related signals") {
		t.Errorf("AGENTS.md lacks its sections:\n%s", b)
	}
}

func TestWorkspace(t *testing.T) {
	b, err := Workspace(root, testEnv())
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	var ws struct {
		Metadata struct{ Name, Atespace string } `yaml:"metadata"`
		Spec     struct {
			Files []struct{ Path, Content string }
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &ws); err != nil {
		t.Fatalf("rendered workspace is not YAML: %v\n%s", err, b)
	}
	md, err := AgentsMD(root, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if ws.Metadata.Name != "demo-ws" || ws.Metadata.Atespace != "agents" || len(ws.Spec.Files) != 1 ||
		ws.Spec.Files[0].Path != "AGENTS.md" || ws.Spec.Files[0].Content != string(md) {
		t.Errorf("workspace = %+v", ws)
	}
}

func TestTokens(t *testing.T) {
	if err := tok.Check(); err != nil {
		t.Errorf("Check(good) = %v", err)
	}
	for _, bad := range []Tokens{
		{Inject: "short", Attach: tok.Attach},
		{Inject: tok.Inject, Attach: strings.Repeat("B", 32)},
		{Inject: tok.Inject, Attach: tok.Inject},
	} {
		if err := bad.Check(); err == nil {
			t.Errorf("Check(%+v) succeeded", bad)
		}
	}
	a, err := NewTokens()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTokens()
	if err != nil {
		t.Fatal(err)
	}
	if a.Check() != nil || b.Check() != nil || a == b {
		t.Errorf("NewTokens = %+v, %+v", a, b)
	}
}

func TestTemplateErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := AgentsMD(dir, testEnv()); err == nil {
		t.Error("AgentsMD with no templates succeeded")
	}
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"AGENTS.md.tmpl":      "{{ .Clusters",
		"task.yaml.tmpl":      "{{ .Missing }}",
		"workspace.yaml.tmpl": "x",
	} {
		if err := os.WriteFile(filepath.Join(dir, Dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AgentsMD(dir, testEnv()); err == nil {
		t.Error("AgentsMD with a broken template succeeded")
	}
	if _, err := Workspace(dir, testEnv()); err == nil {
		t.Error("Workspace with a broken AGENTS.md template succeeded")
	}
	if _, err := Task(dir, testEnv(), tok); err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Errorf("Task with a missing key: err = %v", err)
	}
}

// lookout parses the task template as YAML 1.1 (sigs.k8s.io/yaml), where
// a bare on, no or y is a boolean. Every value is quoted, so a valid
// atespace or workspace named "on" stays a string.
func TestTemplatesSurviveYAML11(t *testing.T) {
	e := testEnv()
	e.Agent.Atespace, e.Agent.Workspace = "on", "no"
	task, err := Task(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := Workspace(root, e)
	if err != nil {
		t.Fatal(err)
	}
	e.Agent.ChatImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("1", 64)
	chat, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"task": task, "workspace": ws, "core-agent chat task": chat} {
		var doc map[string]any
		if err := yaml11.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		md, _ := doc["metadata"].(map[any]any)
		if v, ok := md["atespace"].(string); !ok || v != "on" {
			t.Errorf("%s: atespace = %#v, want the string \"on\"", name, md["atespace"])
		}
	}
}

func TestSwitchboardConfig(t *testing.T) {
	b, err := SwitchboardConfig(root, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Platform     string `json:"platform"`
		DefaultAgent string `json:"default_agent"`
		Agents       []struct {
			Name     string `json:"name"`
			Kind     string `json:"kind"`
			TokenEnv string `json:"token_env"`
			AX       struct {
				Atespace         string `json:"atespace"`
				TaskTemplateFile string `json:"task_template_file"`
				Scope            string `json:"scope"`
			} `json:"ax"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if cfg.Platform != "slack" || len(cfg.Agents) != 1 || cfg.DefaultAgent != "triage" {
		t.Fatalf("config %+v", cfg)
	}
	a := cfg.Agents[0]
	if a.Kind != "ax" || a.AX.Atespace != "agents" || a.AX.Scope != "thread" ||
		a.AX.TaskTemplateFile != "/etc/switchboard/triage.yaml" || a.TokenEnv != "SWITCHBOARD_AGENT_TOKEN_TRIAGE" {
		t.Errorf("agent %+v", a)
	}
}

func TestSwitchboardConfigErrors(t *testing.T) {
	if _, err := SwitchboardConfig(t.TempDir(), testEnv()); err == nil {
		t.Error("missing template accepted")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Dir, "switchboard.json.tmpl"), []byte(`{"atespace": {{ .Atespace }}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SwitchboardConfig(dir, testEnv()); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("broken template: %v", err)
	}
}

// lookout's tasks run the incident bundle and switchboard's the chat
// bundle; both are in the repository and baked into the agent image, so a
// template can't name a bundle the image lacks.
func TestBundles(t *testing.T) {
	inc, err := Task(root, testEnv(), tok)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := ChatTask(root, testEnv(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inc), "--workload=/opt/mast/workloads/"+IncidentBundle+"\n") ||
		!strings.Contains(string(chat), "--workload=/opt/mast/workloads/"+ChatBundle+"\n") {
		t.Errorf("incident template:\n%s\nchat template:\n%s", inc, chat)
	}
	if strings.Replace(string(chat), "workloads/"+ChatBundle+"\n", "workloads/"+IncidentBundle+"\n", 1) != string(inc) {
		t.Error("the two templates differ in more than the bundle")
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "images/agent-mast/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{IncidentBundle, ChatBundle} {
		if _, err := os.Stat(filepath.Join(root, "agents/sre-triage", b, "workload.yaml")); err != nil {
			t.Errorf("bundle %s: %v", b, err)
		}
		if !strings.Contains(string(dockerfile), "COPY agents/sre-triage/"+b+" /opt/mast/workloads/"+b+"\n") {
			t.Errorf("the agent image doesn't carry bundle %s", b)
		}
	}
}

// With agent.chatImage set, switchboard's threads run core-agent (#48):
// its image and command, the attach token under core-agent's name, and
// everything Substrate books or routes on the same as the mast task's.
func TestChatTaskCoreAgent(t *testing.T) {
	type spec struct {
		Image     string
		Command   []string
		Env       []struct{ Name, Value string }
		Egress    any            `yaml:"egress"`
		HTTP      any            `yaml:"http"`
		Idle      any            `yaml:"idle"`
		Resources any            `yaml:"resources"`
		Workspace map[string]any `yaml:"workspace"`
		Debug     bool           `yaml:"debug"`
	}
	parse := func(b []byte) (string, spec) {
		t.Helper()
		var task struct {
			Metadata struct{ Atespace string } `yaml:"metadata"`
			Spec     spec                      `yaml:"spec"`
		}
		if err := yaml.Unmarshal(b, &task); err != nil {
			t.Fatalf("rendered task is not YAML: %v\n%s", err, b)
		}
		return task.Metadata.Atespace, task.Spec
	}

	e := testEnv()
	e.Agent.ChatImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("1", 64)
	b, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	atespace, got := parse(b)
	mastB, err := ChatTask(root, testEnv(), tok)
	if err != nil {
		t.Fatal(err)
	}
	_, mast := parse(mastB)

	envs := map[string]string{}
	for _, v := range got.Env {
		envs[v.Name] = v.Value
	}
	// The whole command line: core-agent's flag forms differ from mast's
	// (--session-db is a bool, --attach-token names an env var), and a
	// mast-style flag fails core-agent's parse at task start.
	wantCmd := []string{
		"core-agent",
		"-c", "/opt/core-agent/recipes/" + ChatBundle + "/.agents/config.json",
		"--provider=vertex", "--model=gemini-3.8-flash", "--no-repl",
		"--attach-listen=127.0.0.1:8484", "--attach-token=CORE_AGENT_ATTACH_TOKEN",
		"--session-db", "--session-db-path=/workspace/.core-agent/sessions.db",
		"--checkpoint=operator", "--disable-tools=bash,read_file,read_many_files,view_file_outline,write_file,edit_file,delete_file,stat,list_dir,glob,grep", "--agentic-tools=false", "--no-background-agents", "--no-pricing-refresh",
		"--agents-content-dir=/workspace/repo",
	}
	if !slices.Equal(got.Command, wantCmd) {
		t.Errorf("command\n got %q\nwant %q", got.Command, wantCmd)
	}
	// images/agent's boot check starts core-agent with the template's
	// flags that shape what it builds (provider, model, tools), so a
	// combination core-agent refuses fails the build, not the task.
	dockerfile, err := os.ReadFile(filepath.Join(root, "images/agent/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var taskFlags []string
	for _, line := range strings.Split(string(dockerfile), "\n") {
		if v, ok := strings.CutPrefix(line, "ARG TASK_FLAGS="); ok {
			taskFlags = strings.Fields(strings.Trim(v, `"`))
		}
	}
	var shaping []string
	for _, f := range wantCmd[3:] { // after core-agent -c <config>
		switch {
		case f == "--no-repl", strings.HasPrefix(f, "--attach-"), strings.HasPrefix(f, "--session-db"), strings.HasPrefix(f, "--agents-content-dir"):
			continue // the listener, its token and paths, which the check sets for itself
		}
		shaping = append(shaping, f)
	}
	if !slices.Equal(taskFlags, shaping) {
		t.Errorf("images/agent's TASK_FLAGS\n got %q\nwant the template's %q", taskFlags, shaping)
	}
	cmd := strings.Join(got.Command, " ")
	switch {
	case atespace != "agents":
		t.Errorf("atespace %q", atespace)
	case got.Image != e.Agent.ChatImage:
		t.Errorf("image %q", got.Image)
	case len(got.Command) == 0 || got.Command[0] != "core-agent":
		t.Errorf("command %q", got.Command)
	case !strings.Contains(cmd, "-c /opt/core-agent/recipes/"+ChatBundle+"/.agents/config.json"):
		t.Errorf("command %q: not the chat bundle", cmd)
	case !strings.Contains(cmd, "--attach-token=CORE_AGENT_ATTACH_TOKEN"):
		t.Errorf("command %q: attach token not read from CORE_AGENT_ATTACH_TOKEN", cmd)
	case envs["CORE_AGENT_ATTACH_TOKEN"] != tok.Attach:
		t.Errorf("attach token %q", envs["CORE_AGENT_ATTACH_TOKEN"])
	case envs["CORE_AGENT_GOOGLE_AUTH"] != "injected" || envs["GOOGLE_CLOUD_PROJECT"] != "example-project" || envs["GOOGLE_CLOUD_LOCATION"] == "":
		t.Errorf("env %v", envs)
	case strings.Contains(string(b), "MAST_"):
		t.Errorf("core-agent task carries mast settings:\n%s", b)
	}
	for name, pair := range map[string][2]any{
		"egress": {got.Egress, mast.Egress}, "http": {got.HTTP, mast.HTTP}, "idle": {got.Idle, mast.Idle},
		"resources": {got.Resources, mast.Resources}, "workspace": {got.Workspace, mast.Workspace}, "debug": {got.Debug, mast.Debug},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s differs from the mast task's: %#v vs %#v", name, pair[0], pair[1])
		}
	}

	// lookout's incidents stay on mast.
	inc, err := Task(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, s := parse(inc); s.Image != e.Agent.Image {
		t.Errorf("incident task image %q, want agent.image", s.Image)
	}
	if _, err := ChatTask(root, e, Tokens{Inject: tok.Inject, Attach: "x"}); err == nil {
		t.Error("ChatTask with a bad token succeeded")
	}
}

// images/agent converts the bundles the tasks name, and the chat bundle's
// attach overlay names the attach token's holder without letting it
// assert anyone: switchboard's assert_caller is off (docs/decisions.md).
func TestCoreAgentImage(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join(root, "images/agent/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "for b in "+IncidentBundle+" "+ChatBundle+"; do") {
		t.Errorf("images/agent doesn't convert %s and %s", IncidentBundle, ChatBundle)
	}
	b, err := os.ReadFile(filepath.Join(root, Dir, "core-agent-"+ChatBundle+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Attach struct {
			MultiSession struct {
				Enabled bool `json:"enabled"`
				Auth    struct {
					TokenIdentity string `json:"token_identity"`
				} `json:"auth"`
				ProxyIdentities []string `json:"proxy_identities"`
			} `json:"multi_session"`
		} `json:"attach"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	ms := o.Attach.MultiSession
	if !ms.Enabled || ms.Auth.TokenIdentity == "" || len(ms.ProxyIdentities) != 0 {
		t.Errorf("attach overlay %+v: want multi-session on the token's identity, and no proxies", ms)
	}
	// No one approves an incident's tool calls: they're bounded.
	b, err = os.ReadFile(filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var inc struct {
		Permissions struct {
			ApprovalTimeout string `json:"approval_timeout"`
		} `json:"permissions"`
		Attach map[string]any `json:"attach"`
	}
	if err := json.Unmarshal(b, &inc); err != nil {
		t.Fatal(err)
	}
	if d, err := time.ParseDuration(inc.Permissions.ApprovalTimeout); err != nil || d <= 0 || d > 2*time.Minute {
		t.Errorf("incident overlay %s: want a short approval_timeout", b)
	}
	if _, ok := inc.Attach["multi_session"]; ok {
		t.Errorf("incident overlay %s: want no multi-session settings", b)
	}

	// Requests reach core-agent's loopback listener from Substrate's
	// router, Host and all (ax-task-runner's pass-through keeps it);
	// core-agent answers any Host it doesn't know 421 (#1358), so both
	// overlays allow the router's names, and the build checks as the
	// router asks.
	routerHosts := []string{"atenet-router.ate-system.svc.cluster.local", "atenet-router.ate-system.svc", "atenet-router.ate-system"}
	for _, bundle := range []string{IncidentBundle, ChatBundle} {
		b, err := os.ReadFile(filepath.Join(root, Dir, "core-agent-"+bundle+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			Attach struct {
				AllowedHosts []string `json:"allowed_hosts"`
			} `json:"attach"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(o.Attach.AllowedHosts, routerHosts) {
			t.Errorf("%s overlay: attach.allowed_hosts %q, want the router's names %q", bundle, o.Attach.AllowedHosts, routerHosts)
		}
	}
	if !strings.Contains(string(dockerfile), "ARG ROUTER_HOST="+routerHosts[0]+"\n") || !strings.Contains(string(dockerfile), `-H "Host: $ROUTER_HOST"`) || !strings.Contains(string(dockerfile), `if [ "$code" != 200 ]; then`) {
		t.Error("images/agent's boot check doesn't ask as the router does")
	}
	// Where mast had the MCP tools: each specialist's own content root,
	// and the main agent only what main-tools names (incidents: none,
	// as mast's coordinator; chat: cluster-level reads for quick answers).
	for _, step := range []string{`grep -qx "root: roots/$n" "$sa"`, `the importer gave the main agent MCP servers`, `/in/ax/core-agent-$b.main-tools.json`, `rm -f "$mcp"`} {
		if !strings.Contains(string(dockerfile), step) {
			t.Errorf("images/agent lacks %q (the MCP layout checks)", step)
		}
	}
	// The folded specialist's root becomes the main agent's servers
	// before main-tools narrows them, and only then is the root removed.
	d := string(dockerfile)
	take, drop, narrow := strings.Index(d, `cp "/in/$b/mcp.json" "$mcp"`), strings.Index(d, `rm -rf "/out/$b/.agents/roots/$n"`), strings.Index(d, `/in/ax/core-agent-$b.main-tools.json`)
	if take < 0 || take > drop || drop > narrow {
		t.Error("images/agent's fold doesn't take the specialist's servers, then remove its root, before narrowing")
	}
	// The incident profile triages itself: its one specialist runs as
	// the main agent (so lookout's signal is read where it lands, not
	// re-typed into a delegation), with that specialist's tools and a
	// reporting section for a main agent in place of finish_task.
	fold, err := os.ReadFile(filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".fold-specialist"))
	if err != nil || strings.TrimSpace(string(fold)) != "_fallback" {
		t.Errorf("incident fold-specialist %q, %v", fold, err)
	}
	spec, err := os.ReadFile(filepath.Join(root, "agents/sre-triage", IncidentBundle, "specialists/_fallback.specialist.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"get_k8s_resource", "describe_k8s_resource", "get_k8s_logs", "list_k8s_events"} {
		if !strings.Contains(string(spec), "        - "+tool+"\n") {
			t.Errorf("_fallback no longer holds %s; the incident main agent's tools would drift from it", tool)
		}
	}
	var incMain []string
	b, err = os.ReadFile(filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".main-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &incMain); err != nil || !slices.Equal(incMain, []string{"get_k8s_resource", "describe_k8s_resource", "get_k8s_logs", "list_k8s_events"}) {
		t.Errorf("incident main agent's tools %q, %v: want _fallback's", incMain, err)
	}
	if !strings.Contains(string(spec), "\nReturn your finding by calling `finish_task`") {
		t.Error("_fallback's finish_task paragraph moved; the fold cuts the instructions there")
	}
	report, err := os.ReadFile(filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".report.md"))
	if err != nil || strings.Contains(string(report), "finish_task") {
		t.Errorf("incident report section: %v, or it names finish_task", err)
	}
	// core-agent enforces agent.max_steps on a turn's main-agent model
	// calls (#1500): the incident cap must fire, with final_report's
	// partial, before the 150s bound cuts the turn off with nothing (a
	// healthy run is 4 calls in 45-75s).
	var incCfg struct {
		Agent struct {
			MaxSteps            int `json:"max_steps"`
			MaxWallclockSeconds int `json:"max_wallclock_seconds"`
		} `json:"agent"`
	}
	b, err = os.ReadFile(filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &incCfg); err != nil || incCfg.Agent.MaxSteps < 5 || incCfg.Agent.MaxSteps*15 > incCfg.Agent.MaxWallclockSeconds {
		t.Errorf("incident agent %+v: want a step cap that fires within the wall-clock bound (about 15s a call)", incCfg.Agent)
	}
	if _, err := os.Stat(filepath.Join(root, Dir, "core-agent-"+ChatBundle+".fold-specialist")); !os.IsNotExist(err) {
		t.Errorf("the chat profile folds its specialist (%v); it delegates", err)
	}
	for _, step := range []string{`fold="/in/ax/core-agent-$b.fold-specialist"`, `/^## Subagent Roster/`, `/^Return your finding by calling ` + "`finish_task`" + `/ { exit }`, `cat "/in/ax/core-agent-$b.report.md"`, `[ -f "$sa" ] || continue`} {
		if !strings.Contains(string(dockerfile), step) {
			t.Errorf("images/agent lacks %q (the fold)", step)
		}
	}
	b, err = os.ReadFile(filepath.Join(root, Dir, "core-agent-"+ChatBundle+".main-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var chatMain []string
	if err := json.Unmarshal(b, &chatMain); err != nil || !slices.Equal(chatMain, []string{"list_clusters", "get_cluster"}) {
		t.Errorf("chat main agent's tools %q, %v", chatMain, err)
	}
	for _, step := range []string{`jq -s '.[0] * .[1]'`, "/in/ax/core-agent-$b.json", "--attach-token=SMOKE_TOKEN"} {
		if !strings.Contains(string(dockerfile), step) {
			t.Errorf("images/agent lacks %q (the overlay merge or the boot check)", step)
		}
	}
}

// agent.incidentImage moves lookout's incidents to core-agent (#48): the
// same template as the chat task's, with the incident bundle.
func TestTaskCoreAgent(t *testing.T) {
	e := testEnv()
	e.Agent.IncidentImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("3", 64)
	b, err := Task(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		Spec struct {
			Image   string
			Command []string
			Env     []struct{ Name, Value string }
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(task.Spec.Command, " ")
	switch {
	case task.Spec.Image != e.Agent.IncidentImage:
		t.Errorf("image %q", task.Spec.Image)
	case task.Spec.Command[0] != "core-agent" || !strings.Contains(cmd, "-c /opt/core-agent/recipes/"+IncidentBundle+"/.agents/config.json "):
		t.Errorf("command %q", cmd)
	case strings.Contains(string(b), "MAST_"):
		t.Errorf("core-agent incident task carries mast settings:\n%s", b)
	}
	// The incident and chat tasks differ in nothing but image and bundle.
	e.Agent.ChatImage = e.Agent.IncidentImage
	chat, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	var gotDoc, chatDoc any
	if err := yaml.Unmarshal(b, &gotDoc); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(strings.Replace(string(chat), "recipes/"+ChatBundle+"/", "recipes/"+IncidentBundle+"/", 1)), &chatDoc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotDoc, chatDoc) {
		t.Error("the core-agent incident and chat tasks differ in more than the bundle")
	}
	// Switchboard's threads don't follow incidentImage.
	e.Agent.ChatImage = ""
	if chat, _ := ChatTask(root, e, tok); strings.Contains(string(chat), e.Agent.IncidentImage) {
		t.Error("incidentImage moved the chat task")
	}
	if _, err := Task(root, e, Tokens{Inject: tok.Inject, Attach: "x"}); err == nil {
		t.Error("Task with a bad token succeeded")
	}
}
