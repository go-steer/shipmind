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
			Atespace:      "agents",
			Project:       "example-project",
			Workspace:     "demo-ws",
			IncidentImage: "registry.example.com/agent@sha256:" + strings.Repeat("0", 64),
			Clusters: []env.Cluster{
				{Name: "demo", Path: "projects/example-project/locations/us-central1/clusters/demo"},
				{Name: "prod", Path: "projects/example-project/locations/europe-west1/clusters/prod"},
			},
		},
	}
}

func TestTask(t *testing.T) {
	e := testEnv()
	b, err := Task(root, e, tok)
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
	for _, ev := range task.Spec.Env {
		envs[ev.Name] = ev.Value
	}
	switch {
	case task.Metadata.Atespace != "agents":
		t.Errorf("atespace %q", task.Metadata.Atespace)
	case task.Spec.Image != e.Agent.IncidentImage:
		t.Errorf("image %q", task.Spec.Image)
	case envs["GOOGLE_CLOUD_PROJECT"] != "example-project":
		t.Errorf("project %q", envs["GOOGLE_CLOUD_PROJECT"])
	case envs["CORE_AGENT_ATTACH_TOKEN"] != tok.Attach || envs["CORE_AGENT_GOOGLE_AUTH"] != "injected":
		t.Errorf("env %v", envs)
	case strings.Contains(string(b), "MAST_"):
		t.Errorf("task carries mast settings:\n%s", b)
	case task.Spec.Workspace.Name != "demo-ws" || task.Spec.Workspace.Path != "/workspace/repo":
		t.Errorf("workspace %+v", task.Spec.Workspace)
	case task.Spec.Resources.Limits.CPU == "" || task.Spec.Resources.Limits.Memory == "":
		// Substrate v1.0 refuses an actor without both.
		t.Errorf("resources %+v", task.Spec.Resources)
	case task.Spec.HTTP.Port != 8484:
		t.Errorf("http.port %d", task.Spec.HTTP.Port)
	}

	if _, err := Task(root, e, Tokens{Inject: "x", Attach: tok.Attach}); err == nil {
		t.Error("Task with a bad token succeeded")
	}
	noImg := testEnv()
	noImg.Agent.IncidentImage = ""
	if _, err := Task(root, noImg, tok); err == nil || !strings.Contains(err.Error(), "agent.incidentImage is required") {
		t.Errorf("Task without agent.incidentImage = %v, want agent.incidentImage required error", err)
	}
}

func TestAgentsMD(t *testing.T) {
	b, err := AgentsMD(root, testEnv())
	if err != nil {
		t.Fatalf("AgentsMD: %v", err)
	}
	md := string(b)
	want := "| cluster | full name |\n|---|---|\n" +
		"| demo | projects/example-project/locations/us-central1/clusters/demo |\n" +
		"| prod | projects/example-project/locations/europe-west1/clusters/prod |\n" +
		"\nIf the cluster is neither"
	if !strings.Contains(md, want) {
		t.Errorf("AGENTS.md lacks the cluster table:\n%s", b)
	}
	if strings.Contains(md, "GKE MCP") || !strings.Contains(md, "`lookout_k8s_*`") {
		t.Errorf("AGENTS.md should reference lookout_k8s_* instead of hosted GKE MCP:\n%s", b)
	}
	_, repeatSection, ok := strings.Cut(md, "# Repeated and related signals\n")
	if !strings.HasPrefix(md, "# Clusters\n") || !ok {
		t.Fatalf("AGENTS.md lacks its sections:\n%s", b)
	}
	repeatNorm := strings.Join(strings.Fields(repeatSection), " ")
	for _, wantSignal := range []string{
		`"kind":"watchboard.digest"`,
		"`sequence`",
		"`entries`",
		"`namespace`",
		"`kind_of_object`",
		"`name`",
		"never by `fingerprint` alone",
		"intervening `resolved`",
		"`watchboard.rotated`",
		"`shipmind.dev/scenario`",
		"finding stands",
	} {
		if !strings.Contains(repeatNorm, wantSignal) {
			t.Errorf("AGENTS.md without AlertChannel lacks %s in # Repeated and related signals:\n%s", wantSignal, repeatSection)
		}
	}
	if strings.Contains(repeatNorm, "(or `fingerprint`)") {
		t.Errorf("# Repeated and related signals must not match watchboard repeats by class fingerprint alone:\n%s", repeatSection)
	}
	for _, unwanted := range []string{
		"# GitOps pull requests",
		"# Slack incident notifications",
	} {
		if strings.Contains(md, unwanted) {
			t.Errorf("AGENTS.md without VCS or AlertChannel should omit %q:\n%s", unwanted, md)
		}
	}

	withChatVCS := testEnv()
	withChatVCS.Agent.VCS = &env.VCS{
		Secret:       "gh-token",
		Repositories: []env.Repository{{Repository: "acme/gitops"}},
	}
	chatVCSBytes, err := AgentsMD(root, withChatVCS)
	if err != nil {
		t.Fatalf("AgentsMD(with chat VCS only): %v", err)
	}
	chatVCSMD := string(chatVCSBytes)
	if !strings.Contains(chatVCSMD, "# GitOps pull requests") ||
		strings.Contains(chatVCSMD, "On an autonomous incident") ||
		strings.Contains(chatVCSMD, "# Slack incident notifications") {
		t.Errorf("AGENTS.md with IncidentAutoPR=false has wrong sections:\n%s", chatVCSMD)
	}

	withAutoPR := testEnv()
	withAutoPR.Agent.VCS = &env.VCS{
		Secret:         "gh-token",
		IncidentAutoPR: true,
		Repositories:   []env.Repository{{Repository: "acme/gitops"}},
	}
	autoPRBytes, err := AgentsMD(root, withAutoPR)
	if err != nil {
		t.Fatalf("AgentsMD(with IncidentAutoPR, no AlertChannel): %v", err)
	}
	autoPRMD := string(autoPRBytes)
	_, gitopsSection, ok := strings.Cut(autoPRMD, "# GitOps pull requests\n")
	if !ok || strings.Contains(autoPRMD, "# Slack incident notifications") {
		t.Fatalf("AGENTS.md with IncidentAutoPR only has wrong optional sections:\n%s", autoPRMD)
	}
	gitopsNorm := strings.Join(strings.Fields(gitopsSection), " ")
	for _, wantGitOps := range []string{
		"On an autonomous incident",
		"`shipmind.dev/scenario` (on the object or its owning workload)",
		"`watchboard.rotated`",
	} {
		if !strings.Contains(gitopsNorm, wantGitOps) {
			t.Errorf("# GitOps pull requests with IncidentAutoPR lacks %s:\n%s", wantGitOps, gitopsSection)
		}
	}

	withAlert := testEnv()
	withAlert.Slack = &env.Slack{App: "shipmind", Command: "/shipmind", AlertChannel: "C0123456789"}
	alertBytes, err := AgentsMD(root, withAlert)
	if err != nil {
		t.Fatalf("AgentsMD(with AlertChannel): %v", err)
	}
	alertMD := string(alertBytes)
	_, slackSection, ok := strings.Cut(alertMD, "# Slack incident notifications\n")
	if !ok {
		t.Fatalf("AGENTS.md with AlertChannel lacks # Slack incident notifications:\n%s", alertMD)
	}
	slackNorm := strings.Join(strings.Fields(slackSection), " ")
	for _, wantSlack := range []string{
		"call `alert` once on the initial diagnosis",
		`"kind":"watchboard.digest"`,
		"`sequence`",
		"`shipmind.dev/scenario`",
		"never matched by `fingerprint` alone",
		"intervening `resolved`",
		"once for that digest batch",
		"finding stands",
		"`watchboard.rotated`",
	} {
		if !strings.Contains(slackNorm, wantSlack) {
			t.Errorf("# Slack incident notifications lacks %s:\n%s", wantSlack, slackSection)
		}
	}
	if strings.Contains(alertMD, "# GitOps pull requests") {
		t.Errorf("AGENTS.md with AlertChannel only should omit # GitOps pull requests:\n%s", alertMD)
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
		"AGENTS.md.tmpl":            "{{ .Clusters",
		"task-core-agent.yaml.tmpl": "{{ .Missing }}",
		"workspace.yaml.tmpl":       "x",
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
	e.Agent.IncidentImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("2", 64)
	chat, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	e.Agent.VCS = &env.VCS{Secret: "no", IncidentAutoPR: true, Repositories: []env.Repository{{Repository: "on/no", BaseBranch: "on"}}}
	e.Slack = &env.Slack{App: "shipmind", Command: "/shipmind", AlertChannel: "C0123456789"}
	chatVCS, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	incFull, err := Task(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	wsVCS, err := Workspace(root, e)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"task": task, "workspace": ws, "core-agent chat task": chat, "chat task with vcs": chatVCS, "incident task with vcs+alert": incFull, "workspace with vcs+alert": wsVCS} {
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

// Both lookout's incident tasks (gke-triage-f) and switchboard's chat tasks
// (gke-chat) run native flat core-agent recipes in images/agent, so no
// template can name a recipe the image lacks.
func TestBundles(t *testing.T) {
	inc, err := Task(root, testEnv(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inc), "/opt/core-agent/recipes/"+IncidentBundle+"/.agents/config.json\n") {
		t.Errorf("incident template:\n%s", inc)
	}
	if _, err := ChatTask(root, testEnv(), tok); err == nil || !strings.Contains(err.Error(), "agent.chatImage is required") {
		t.Errorf("ChatTask without agent.chatImage = %v, want agent.chatImage required error", err)
	}
	for _, bundle := range []string{IncidentBundle, ChatBundle} {
		for _, f := range []string{"AGENTS.md", "config.json", "mcp.json"} {
			if _, err := os.Stat(filepath.Join(root, "agents/sre-triage", bundle, ".agents", f)); err != nil {
				t.Errorf("native %s recipe missing .agents/%s: %v", bundle, f, err)
			}
		}
	}
}

// Switchboard's threads run core-agent with agent.chatImage: its image and
// command, the attach token under core-agent's name, and the same Substrate
// booking and routing fields as the incident task.
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
	incB, err := Task(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	_, inc := parse(incB)

	envs := map[string]string{}
	for _, v := range got.Env {
		envs[v.Name] = v.Value
	}
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
	case envs["CORE_AGENT_TRUSTED_DIRS"] != "/workspace/repo,/opt/core-agent/recipes/"+ChatBundle+"/.agents":
		t.Errorf("CORE_AGENT_TRUSTED_DIRS = %q", envs["CORE_AGENT_TRUSTED_DIRS"])
	case strings.Contains(string(b), "MAST_"):
		t.Errorf("core-agent task carries mast settings:\n%s", b)
	}
	wantChatEgress := []any{map[string]any{
		"hosts": []any{"aiplatform.googleapis.com", "container.googleapis.com", "*.europe-west1.gke.goog", "*.us-central1.gke.goog"},
		"credentials": []any{map[string]any{
			"header":        "Authorization",
			"prefix":        "Bearer ",
			"credentialUri": "ate-secret://k8s.io/default/ate-credentials/google-token/token",
		}},
	}}
	if !reflect.DeepEqual(got.Egress, wantChatEgress) {
		t.Errorf("chat egress = %#v, want %#v", got.Egress, wantChatEgress)
	}
	for name, pair := range map[string][2]any{
		"http": {got.HTTP, inc.HTTP}, "idle": {got.Idle, inc.Idle},
		"resources": {got.Resources, inc.Resources}, "workspace": {got.Workspace, inc.Workspace}, "debug": {got.Debug, inc.Debug},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s differs from the incident task's: %#v vs %#v", name, pair[0], pair[1])
		}
	}

	// Incident tasks keep their own image pin independent of chatImage.
	if inc.Image != e.Agent.IncidentImage {
		t.Errorf("incident task image %q, want agent.incidentImage", inc.Image)
	}
	if _, err := ChatTask(root, e, Tokens{Inject: tok.Inject, Attach: "x"}); err == nil {
		t.Error("ChatTask with a bad token succeeded")
	}
}

// images/agent copies native gke-triage-f/.agents and gke-chat/.agents
// directly (with pinned k8s-lookout skills and zero import-workload/fold
// machinery), builds core-agent with -tags no_tui (declining headless MCP
// elicitation and dropping the unused TUI), and boot-checks both recipes.
func TestCoreAgentImage(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join(root, "images/agent/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	d := string(dockerfile)
	if strings.Count(d, "go build -tags no_tui ") != 2 {
		t.Errorf("images/agent/Dockerfile must build both core-agent binaries with -tags no_tui (found %d)", strings.Count(d, "go build -tags no_tui "))
	}
	for _, want := range []string{
		"COPY --from=lookout /src/skills /lookout-skills",
		"cp -a /in/" + IncidentBundle + "/.agents/. /out/" + IncidentBundle + "/.agents/",
		"cp -a /in/" + ChatBundle + "/.agents/. /out/" + ChatBundle + "/.agents/",
		"cp -a /lookout-skills/k8s-triage /lookout-skills/playbooks /out/" + IncidentBundle + "/.agents/skills/",
		"cp -a /lookout-skills/k8s-triage /lookout-skills/cluster-health /lookout-skills/k8s-audit /lookout-skills/k8s-capacity /lookout-skills/gitops-drift /lookout-skills/playbooks /out/" + ChatBundle + "/.agents/skills/",
		"/out/" + IncidentBundle + "/.agents/skills/k8s-triage/references/playbooks",
		"/out/" + ChatBundle + "/.agents/skills/k8s-triage/references/playbooks",
		"for b in " + IncidentBundle + " " + ChatBundle + "; do",
		"--attach-token=SMOKE_TOKEN",
		"builtin:propose-change",
		"skills: 1 loaded — k8s-triage",
		"skills: 5 loaded — cluster-health, gitops-drift, k8s-audit, k8s-capacity, k8s-triage",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("images/agent/Dockerfile lacks %q", want)
		}
	}
	for _, unwanted := range []string{
		"import-workload",
		"fold-specialist",
		"main-tools.json",
		"core-agent-$b.base.json",
		"core-agent-$b.report.md",
	} {
		if strings.Contains(d, unwanted) {
			t.Errorf("images/agent/Dockerfile must not contain legacy mast-import machinery %q", unwanted)
		}
	}

	b, err := os.ReadFile(filepath.Join(root, "agents/sre-triage", ChatBundle, ".agents", "config.json"))
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
		t.Errorf("chat config attach %+v: want multi-session on the token's identity, and no proxies", ms)
	}
	// No one approves an incident's tool calls: they're bounded.
	b, err = os.ReadFile(filepath.Join(root, "agents/sre-triage", IncidentBundle, ".agents", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inc struct {
		Agent struct {
			MaxSteps            int  `json:"max_steps"`
			MaxWallclockSeconds int  `json:"max_wallclock_seconds"`
			FinalReport         bool `json:"final_report"`
		} `json:"agent"`
		Permissions struct {
			Mode            string   `json:"mode"`
			ApprovalTimeout string   `json:"approval_timeout"`
			Allow           []string `json:"allow"`
		} `json:"permissions"`
		Attach map[string]any `json:"attach"`
	}
	if err := json.Unmarshal(b, &inc); err != nil {
		t.Fatal(err)
	}
	if d, err := time.ParseDuration(inc.Permissions.ApprovalTimeout); err != nil || d <= 0 || d > 2*time.Minute {
		t.Errorf("incident config %s: want a short approval_timeout", b)
	}
	if _, ok := inc.Attach["multi_session"]; ok {
		t.Errorf("incident config %s: want no multi-session settings", b)
	}
	// core-agent enforces agent.max_steps on a turn's main-agent model
	// calls (#1500): the incident cap must fire, with final_report's
	// partial, before the 150s bound cuts the turn off with nothing.
	if !inc.Agent.FinalReport || inc.Agent.MaxSteps < 5 || inc.Agent.MaxSteps*15 > inc.Agent.MaxWallclockSeconds {
		t.Errorf("incident agent %+v: want final_report and a step cap that fires within the wall-clock bound", inc.Agent)
	}
	wantIncAllow := []string{
		"mcp:lookout_k8s_list_clusters*",
		"mcp:lookout_k8s_triage_workload*",
		"mcp:lookout_k8s_resource_spec*",
		"mcp:lookout_k8s_triage_logs*",
		"mcp:lookout_k8s_event_timeline*",
		"mcp:lookout_k8s_list_resources*",
		"mcp:lookout_k8s_state_edges*",
		"mcp:lookout_k8s_triage_delta*",
		"skill:list_skills*",
		"skill:load_skill*",
		"skill:load_skill_resource*",
	}
	if inc.Permissions.Mode != "ask" || !slices.Equal(inc.Permissions.Allow, wantIncAllow) {
		t.Errorf("incident permissions = %+v, want ask with %q", inc.Permissions, wantIncAllow)
	}

	// Requests reach core-agent's loopback listener from Substrate's
	// router, Host and all (ax-task-runner's pass-through keeps it);
	// core-agent answers any Host it doesn't know 421 (#1358), so both
	// recipes allow the router's names, and the build checks as the
	// router asks.
	routerHosts := []string{"atenet-router.ate-system.svc.cluster.local", "atenet-router.ate-system.svc", "atenet-router.ate-system"}
	for _, bundle := range []string{IncidentBundle, ChatBundle} {
		cfgPath := filepath.Join(root, "agents/sre-triage", bundle, ".agents", "config.json")
		b, err := os.ReadFile(cfgPath)
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
			t.Errorf("%s config: attach.allowed_hosts %q, want the router's names %q", bundle, o.Attach.AllowedHosts, routerHosts)
		}
	}
	if !strings.Contains(d, "ARG ROUTER_HOST="+routerHosts[0]+"\n") || !strings.Contains(d, `-H "Host: $ROUTER_HOST"`) || !strings.Contains(d, `if [ "$code" != 200 ]; then`) {
		t.Error("images/agent's boot check doesn't ask as the router does")
	}

	// Legacy mast-import glue for both gke-chat and gke-triage-f (and the
	// retired stock gke-triage bundle) must stay deleted (#131, #144).
	for _, obsolete := range []string{
		filepath.Join(root, "agents/sre-triage", ChatBundle, "workload.yaml"),
		filepath.Join(root, "agents/sre-triage", ChatBundle, "mcp.json"),
		filepath.Join(root, "agents/sre-triage", ChatBundle, "specialists"),
		filepath.Join(root, "agents/sre-triage", ChatBundle, "schemas"),
		filepath.Join(root, Dir, "core-agent-"+ChatBundle+".base.json"),
		filepath.Join(root, Dir, "core-agent-"+ChatBundle+".json"),
		filepath.Join(root, Dir, "core-agent-"+ChatBundle+".main-tools.json"),
		filepath.Join(root, Dir, "core-agent-"+ChatBundle+".fold-specialist"),
		filepath.Join(root, "agents/sre-triage", IncidentBundle, "workload.yaml"),
		filepath.Join(root, "agents/sre-triage", IncidentBundle, "mcp.json"),
		filepath.Join(root, "agents/sre-triage", IncidentBundle, "specialists"),
		filepath.Join(root, "agents/sre-triage", IncidentBundle, "schemas"),
		filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".base.json"),
		filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".json"),
		filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".main-tools.json"),
		filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".fold-specialist"),
		filepath.Join(root, Dir, "core-agent-"+IncidentBundle+".report.md"),
		filepath.Join(root, "agents/sre-triage/gke-triage"),
	} {
		if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
			t.Errorf("obsolete mast-import artifact still exists: %s (%v)", obsolete, err)
		}
	}
	// Retired mast image, workflow, and task template must stay deleted (#48).
	for _, obsolete := range []string{
		filepath.Join(root, "images/agent-mast"),
		filepath.Join(root, ".github/workflows/image-agent-mast.yml"),
		filepath.Join(root, Dir, "task.yaml.tmpl"),
	} {
		if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
			t.Errorf("retired mast artifact still exists: %s (%v)", obsolete, err)
		}
	}
}

func TestChatAndIncidentVCSAndAlertFlows(t *testing.T) {
	e := testEnv()
	if b, err := ChatConfig(root, e); err != nil || b != nil {
		t.Fatalf("ChatConfig without vcs = %s, %v", b, err)
	}
	if b, err := IncidentConfig(root, e); err != nil || b != nil {
		t.Fatalf("IncidentConfig without vcs/alert = %s, %v", b, err)
	}
	e.Agent.ChatImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("4", 64)
	e.Agent.IncidentImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("5", 64)
	e.Agent.VCS = &env.VCS{
		Secret: "gh-token",
		Repositories: []env.Repository{
			{Repository: "acme/gitops"},
			{Repository: "acme/apps", BaseBranch: "release"},
		},
	}

	chatBytes, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatalf("ChatTask: %v", err)
	}
	s := string(chatBytes)
	for _, want := range []string{
		"-c\n    - /workspace/repo/chat-config.json\n",
		"--agents-dir=/opt/core-agent/recipes/" + ChatBundle + "/.agents",
		"--disable-tools=bash,read_many_files,view_file_outline,delete_file,stat,list_dir,glob",
		"CORE_AGENT_TRUSTED_DIRS",
		"ate-secret://k8s.io/default/ate-credentials/gh-token/bearer",
		"ate-secret://k8s.io/default/ate-credentials/gh-token/basic",
		"debug: false",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ChatTask with vcs lacks %q:\n%s", want, s)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "images/agent/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var vcsFlags []string
	for _, line := range strings.Split(string(dockerfile), "\n") {
		if v, ok := strings.CutPrefix(line, "ARG VCS_TASK_FLAGS="); ok {
			vcsFlags = strings.Fields(strings.Trim(v, `"`))
		}
	}
	var parsedTask struct {
		Spec struct{ Command []string } `yaml:"spec"`
	}
	if err := yaml.Unmarshal(chatBytes, &parsedTask); err != nil {
		t.Fatal(err)
	}
	var shaping []string
	for _, f := range parsedTask.Spec.Command[3:] {
		switch {
		case f == "--no-repl", strings.HasPrefix(f, "--agents-dir="), strings.HasPrefix(f, "--attach-"), strings.HasPrefix(f, "--session-db"), strings.HasPrefix(f, "--agents-content-dir"):
			continue
		}
		shaping = append(shaping, f)
	}
	if !slices.Equal(vcsFlags, shaping) {
		t.Errorf("images/agent's VCS_TASK_FLAGS\n got %q\nwant the template's %q", vcsFlags, shaping)
	}

	multiCfg, err := ChatConfig(root, e)
	if err != nil || !strings.Contains(string(multiCfg), `"role": "managed"`) || strings.Contains(string(multiCfg), `"role": "gitops"`) {
		t.Errorf("multi-repo ChatConfig should use role=managed: %s, %v", multiCfg, err)
	}
	if !strings.Contains(string(multiCfg), `"name": "propose-change"`) ||
		!strings.Contains(string(multiCfg), `"spawn_agent:propose-change"`) ||
		!strings.Contains(string(multiCfg), `"mcp": []`) ||
		!strings.Contains(string(multiCfg), `"skills": []`) ||
		!strings.Contains(string(multiCfg), `"vcs_commit:*"`) ||
		strings.Contains(string(multiCfg), `"vcs_publish:*"`) ||
		strings.Contains(string(multiCfg), `"vcs_propose:*"`) {
		t.Errorf("ChatConfig subagent or ask-mode permissions unexpected:\n%s", multiCfg)
	}

	singleEnv := *e
	singleEnv.Agent.VCS = &env.VCS{Secret: "gh-token", Repositories: []env.Repository{{Repository: "acme/gitops"}}}
	singleCfg, err := ChatConfig(root, &singleEnv)
	if err != nil || !strings.Contains(string(singleCfg), `"role": "gitops"`) {
		t.Errorf("single-repo ChatConfig should use role=gitops: %s, %v", singleCfg, err)
	}

	autoApproveEnv := *e
	autoApproveEnv.Agent.VCS = &env.VCS{
		Secret:          "gh-token",
		ChatAutoApprove: true,
		Repositories:    []env.Repository{{Repository: "acme/gitops"}},
	}
	autoApproveCfg, err := ChatConfig(root, &autoApproveEnv)
	if err != nil ||
		!strings.Contains(string(autoApproveCfg), `"vcs_commit:*"`) ||
		!strings.Contains(string(autoApproveCfg), `"vcs_publish:*"`) ||
		!strings.Contains(string(autoApproveCfg), `"vcs_propose:*"`) {
		t.Errorf("ChatConfig with ChatAutoApprove=true should allowlist vcs_publish and vcs_propose:\n%s, %v", autoApproveCfg, err)
	}
	if _, err := ChatConfig(t.TempDir(), e); err == nil {
		t.Error("ChatConfig with missing base JSON succeeded")
	}

	// Alert-only incident (Flow 3 outbound half: incidentAutoPR false, alertChannel set).
	e.Slack = &env.Slack{App: "shipmind", Command: "/shipmind", AlertChannel: "C0123456789"}
	alertTask, err := Task(root, e, tok)
	if err != nil {
		t.Fatalf("Task(alert-only): %v", err)
	}
	alertTaskStr := string(alertTask)
	for _, want := range []string{
		"-c\n    - /workspace/repo/incident-config.json\n",
		"--agents-dir=/opt/core-agent/recipes/" + IncidentBundle + "/.agents",
		"--disable-tools=bash,read_file,read_many_files,view_file_outline,write_file,edit_file,delete_file,stat,list_dir,glob,grep",
		"CORE_AGENT_SWITCHBOARD_TOKEN",
		`http://switchboard.switchboard.svc.cluster.local`,
		"ate-secret://k8s.io/default/ate-credentials/switchboard-ingress/token",
		"debug: true",
	} {
		if !strings.Contains(alertTaskStr, want) {
			t.Errorf("alert-only Task lacks %q:\n%s", want, alertTaskStr)
		}
	}
	if strings.Contains(alertTaskStr, "api.github.com") {
		t.Errorf("alert-only Task with incidentAutoPR=false should not have GitHub egress:\n%s", alertTaskStr)
	}
	alertOnlyCfg, err := IncidentConfig(root, e)
	if err != nil {
		t.Fatalf("IncidentConfig(alert-only): %v", err)
	}
	if !strings.Contains(string(alertOnlyCfg), `"max_steps": 8`) ||
		!strings.Contains(string(alertOnlyCfg), `"max_wallclock_seconds": 150`) ||
		strings.Contains(string(alertOnlyCfg), `"propose-change"`) {
		t.Errorf("alert-only IncidentConfig should keep 150s cap and omit propose-change:\n%s", alertOnlyCfg)
	}

	// Full autonomous incident PR + Slack posting (Flows 1 & 2: incidentAutoPR true + alertChannel set).
	e.Agent.VCS.IncidentAutoPR = true
	incTask, err := Task(root, e, tok)
	if err != nil {
		t.Fatalf("Task(autoPR+alert): %v", err)
	}
	for _, want := range []string{
		"-c\n    - /workspace/repo/incident-config.json\n",
		"--disable-tools=bash,read_many_files,view_file_outline,delete_file,stat,list_dir,glob",
		"ate-secret://k8s.io/default/ate-credentials/gh-token/bearer",
		`http://switchboard.switchboard.svc.cluster.local`,
		"debug: false",
	} {
		if !strings.Contains(string(incTask), want) {
			t.Errorf("autoPR+alert Task lacks %q:\n%s", want, incTask)
		}
	}
	incCfg, err := IncidentConfig(root, e)
	if err != nil {
		t.Fatalf("IncidentConfig: %v", err)
	}
	for _, want := range []string{
		`"max_steps": 12`,
		`"max_wallclock_seconds": 240`,
		`"name": "propose-change"`,
		`"skills": []`,
		`"spawn_agent:propose-change"`,
		`"mcp:lookout_k8s_triage_workload*"`,
		`"mcp:lookout_k8s_triage_logs*"`,
		`"skill:load_skill*"`,
		`"vcs_publish:*"`,
		`"vcs_propose:*"`,
		`"alert:slack"`,
		`"template": "switchboard"`,
		`"conversation": "C0123456789"`,
	} {
		if !strings.Contains(string(incCfg), want) {
			t.Errorf("IncidentConfig lacks %q:\n%s", want, incCfg)
		}
	}
	if strings.Contains(string(incCfg), "mcp:gke_") {
		t.Errorf("IncidentConfig must not carry hosted GKE MCP permissions:\n%s", incCfg)
	}
	if _, err := IncidentConfig(t.TempDir(), e); err == nil {
		t.Error("IncidentConfig with missing base JSON succeeded")
	}

	swBytes, err := SwitchboardConfig(root, e)
	if err != nil {
		t.Fatalf("SwitchboardConfig: %v", err)
	}
	if strings.Contains(string(swBytes), `"name": "propose"`) {
		t.Errorf("SwitchboardConfig still defines a separate propose agent:\n%s", swBytes)
	}
	for _, want := range []string{
		`"name": "triage"`,
		`"ingress_addr": ":8080"`,
		`"ingress_token_env": "SWITCHBOARD_INGRESS_TOKEN"`,
		`"ingress_allow": ["C0123456789"]`,
	} {
		if !strings.Contains(string(swBytes), want) {
			t.Errorf("SwitchboardConfig lacks %q:\n%s", want, swBytes)
		}
	}

	wsBytes, err := Workspace(root, e)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	var ws struct {
		Spec struct {
			Files []struct{ Path, Content string }
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(wsBytes, &ws); err != nil {
		t.Fatalf("Workspace YAML: %v", err)
	}
	if len(ws.Spec.Files) != 3 ||
		ws.Spec.Files[0].Path != "AGENTS.md" ||
		ws.Spec.Files[1].Path != "chat-config.json" ||
		ws.Spec.Files[2].Path != "incident-config.json" ||
		!strings.Contains(ws.Spec.Files[0].Content, "# GitOps pull requests") ||
		!strings.Contains(ws.Spec.Files[0].Content, "# Slack incident notifications") {
		t.Errorf("Workspace files = %+v", ws.Spec.Files)
	}
}

// agent.incidentImage runs lookout's incidents on core-agent (#48, #131): the
// same template as the chat task's, with the incident bundle, trusted recipe
// root (for stdio lookout mcp and bundled skills), and GKE DNS control-plane
// egress for every configured cluster.
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
			Egress  any `yaml:"egress"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	envs := map[string]string{}
	for _, ev := range task.Spec.Env {
		envs[ev.Name] = ev.Value
	}
	cmd := strings.Join(task.Spec.Command, " ")
	switch {
	case task.Spec.Image != e.Agent.IncidentImage:
		t.Errorf("image %q", task.Spec.Image)
	case task.Spec.Command[0] != "core-agent" || !strings.Contains(cmd, "-c /opt/core-agent/recipes/"+IncidentBundle+"/.agents/config.json "):
		t.Errorf("command %q", cmd)
	case strings.Contains(string(b), "MAST_"):
		t.Errorf("core-agent incident task carries mast settings:\n%s", b)
	case envs["CORE_AGENT_TRUSTED_DIRS"] != "/workspace/repo,/opt/core-agent/recipes/"+IncidentBundle+"/.agents":
		t.Errorf("incident task CORE_AGENT_TRUSTED_DIRS = %q", envs["CORE_AGENT_TRUSTED_DIRS"])
	}
	wantIncidentEgress := []any{map[string]any{
		"hosts": []any{"aiplatform.googleapis.com", "container.googleapis.com", "*.europe-west1.gke.goog", "*.us-central1.gke.goog"},
		"credentials": []any{map[string]any{
			"header":        "Authorization",
			"prefix":        "Bearer ",
			"credentialUri": "ate-secret://k8s.io/default/ate-credentials/google-token/token",
		}},
	}}
	if !reflect.DeepEqual(task.Spec.Egress, wantIncidentEgress) {
		t.Errorf("incident egress = %#v, want %#v", task.Spec.Egress, wantIncidentEgress)
	}
	// On a single-project environment, the incident and chat tasks differ only
	// in recipe name (both trust their baked recipe root and carry the same GKE
	// DNS control-plane egress for lookout mcp; #131).
	e.Agent.ChatImage = e.Agent.IncidentImage
	chat, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	normChat := strings.ReplaceAll(string(chat), "recipes/"+ChatBundle+"/", "recipes/"+IncidentBundle+"/")
	var gotDoc, chatDoc any
	if err := yaml.Unmarshal(b, &gotDoc); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(normChat), &chatDoc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotDoc, chatDoc) {
		t.Error("the core-agent incident and chat tasks differ beyond recipe bundle")
	}
	// Switchboard's threads don't follow incidentImage (and refuse to render without chatImage).
	e.Agent.ChatImage = ""
	if chat, err := ChatTask(root, e, tok); err == nil || strings.Contains(string(chat), e.Agent.IncidentImage) {
		t.Errorf("ChatTask without chatImage = %s, %v; want error", chat, err)
	}
	if _, err := Task(root, e, Tokens{Inject: tok.Inject, Attach: "x"}); err == nil {
		t.Error("Task with a bad token succeeded")
	}
}

// Both chat and incident task templates take the environment's idle timeout,
// 10m by default.
func TestTaskIdleSuspend(t *testing.T) {
	withBoth := func(idle string) *env.Env {
		e := testEnv()
		e.Agent.ChatImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("1", 64)
		e.Agent.IdleSuspendAfter = idle
		return e
	}
	for _, tc := range []struct {
		name string
		e    *env.Env
		want string
	}{
		{"default", withBoth(""), "suspendAfter: 10m\n"},
		{"3m", withBoth("3m"), "suspendAfter: 3m\n"},
	} {
		for kind, render := range map[string]func(string, *env.Env, Tokens) ([]byte, error){"chat": ChatTask, "incident": Task} {
			b, err := render(root, tc.e, tok)
			if err != nil {
				t.Fatalf("%s, %s: %v", tc.name, kind, err)
			}
			if !strings.Contains(string(b), tc.want) {
				t.Errorf("%s, %s: rendered task lacks %q", tc.name, kind, tc.want)
			}
		}
	}
}

func TestPostureBundleAndImageChecks(t *testing.T) {
	dockerfileBytes, err := os.ReadFile(filepath.Join(root, "images/agent/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(dockerfileBytes)
	for _, want := range []string{
		"AS lookout",
		"-tags allproviders",
		"/usr/local/bin/lookout version",
		"COPY --from=lookout /out/lookout /usr/local/bin/lookout",
		"COPY --from=lookout /src/skills /lookout-skills",
		"AS leeway",
		"/usr/local/bin/leeway version",
		"COPY --from=leeway /out/leeway /usr/local/bin/leeway",
		"lookout $(jq -r '.servers.lookout.args | join(\" \")' /in/gke-triage-f/.agents/mcp.json) --list-tools",
		"grep -q '^8 tools ' /tmp/lookout-triage-tools",
		"if grep -q '^k8s_scan ' /tmp/lookout-triage-tools; then",
		"lookout $(jq -r '.servers.lookout.args | join(\" \")' /in/gke-chat/.agents/mcp.json) --list-tools",
		"grep -q '^9 tools ' /tmp/lookout-chat-tools",
		"if grep -q '^k8s_scan ' /tmp/lookout-chat-tools; then",
		"leeway $(jq -r '.servers.leeway.args | join(\" \")' /in/gke-chat/.agents/mcp.json)",
		"gcr.io/distroless/static-debian12:nonroot@",
		"tools allowlist names |has no tool matching ",
		"mcp: [0-9]+ server\\(s\\), [1-9][0-9]* down",
		"mcp: [0-9]+ server\\(s\\) loaded .*\\[[1-9][0-9]* failed",
		`instruction: loaded [1-9][0-9]* file\(s\): .*/out/${recipe}/\.agents/AGENTS\.md`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("images/agent/Dockerfile lacks %q", want)
		}
	}
	// The image carries the components' own binaries and no shipmind
	// wrapper in front of them (go-steer/leeway#37, go-steer/k8s-lookout#858).
	if strings.Contains(dockerfile, "/usr/local/bin/shipmind") || strings.Contains(dockerfile, "AS shipmind") {
		t.Error("images/agent/Dockerfile builds or installs shipmind; the agent image runs lookout and leeway directly")
	}

	buildArgsBytes, err := os.ReadFile(filepath.Join(root, "images/agent/build-args.sh"))
	if err != nil {
		t.Fatal(err)
	}
	buildArgs := string(buildArgsBytes)
	for _, want := range []string{"LOOKOUT_REPO=", "LOOKOUT_VERSION=", "LOOKOUT_COMMIT=", "LEEWAY_REPO=", "LEEWAY_VERSION=", "LEEWAY_COMMIT="} {
		if !strings.Contains(buildArgs, want) {
			t.Errorf("images/agent/build-args.sh lacks %q", want)
		}
	}

	type mcpDoc struct {
		Version int `json:"version"`
		Servers map[string]struct {
			Transport    string   `json:"transport"`
			URL          string   `json:"url"`
			Command      string   `json:"command"`
			Args         []string `json:"args"`
			Tools        []string `json:"tools"`
			AgenticNever bool     `json:"agentic_never"`
		} `json:"servers"`
	}

	// Both recipes drop hosted GKE MCP completely (#131, D4=B1).
	incMCPBytes, err := os.ReadFile(filepath.Join(root, "agents/sre-triage", IncidentBundle, ".agents", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(incMCPBytes), "container.googleapis.com/mcp") || strings.Contains(string(incMCPBytes), "k8s_scan") {
		t.Errorf("gke-triage-f/.agents/mcp.json must not reference hosted GKE MCP or k8s_scan:\n%s", incMCPBytes)
	}
	var incMCP mcpDoc
	if err := json.Unmarshal(incMCPBytes, &incMCP); err != nil {
		t.Fatal(err)
	}
	if incMCP.Version != 1 || len(incMCP.Servers) != 1 {
		t.Fatalf("gke-triage-f mcp.json unexpected version/servers: %+v", incMCP)
	}
	if _, hasGKE := incMCP.Servers["gke"]; hasGKE {
		t.Errorf("gke-triage-f mcp.json must not define hosted gke server")
	}
	wantIncLookoutArgs := []string{
		"mcp",
		"--tools=k8s_triage_workload,k8s_resource_spec,k8s_triage_logs,k8s_event_timeline,k8s_list_resources,k8s_state_edges,k8s_triage_delta",
		"--clusters-from=auto",
		"--max-concurrency-per-cluster=8",
	}
	if got := incMCP.Servers["lookout"]; got.Transport != "stdio" || got.Command != "/usr/local/bin/lookout" ||
		!slices.Equal(got.Args, wantIncLookoutArgs) || got.AgenticNever {
		t.Errorf("gke-triage-f lookout MCP server = %+v", got)
	}

	chatMCPBytes, err := os.ReadFile(filepath.Join(root, "agents/sre-triage", ChatBundle, ".agents", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chatMCPBytes), "container.googleapis.com/mcp") || strings.Contains(string(chatMCPBytes), "k8s_scan") {
		t.Errorf("gke-chat/.agents/mcp.json must not reference hosted GKE MCP or k8s_scan:\n%s", chatMCPBytes)
	}
	var chatMCP mcpDoc
	if err := json.Unmarshal(chatMCPBytes, &chatMCP); err != nil {
		t.Fatal(err)
	}
	if chatMCP.Version != 1 || len(chatMCP.Servers) != 2 {
		t.Fatalf("gke-chat mcp.json unexpected version/servers: %+v", chatMCP)
	}
	if _, hasGKE := chatMCP.Servers["gke"]; hasGKE {
		t.Errorf("gke-chat mcp.json must not define hosted gke server")
	}
	wantChatLookoutArgs := []string{
		"mcp",
		"--tools=k8s_audit_all,k8s_triage_workload,k8s_resource_spec,k8s_triage_logs,k8s_event_timeline,k8s_list_resources,k8s_state_edges,k8s_triage_delta",
		"--clusters-from=auto",
		"--max-concurrency-per-cluster=8",
	}
	if got := chatMCP.Servers["lookout"]; got.Transport != "stdio" || got.Command != "/usr/local/bin/lookout" ||
		!slices.Equal(got.Args, wantChatLookoutArgs) || got.AgenticNever {
		t.Errorf("gke-chat lookout MCP server = %+v", got)
	}
	if got := chatMCP.Servers["leeway"]; got.Transport != "stdio" || got.Command != "/usr/local/bin/leeway" ||
		!slices.Equal(got.Args, []string{"mcp", "--timeout=20s", "--parallelism=32"}) || !got.AgenticNever {
		t.Errorf("gke-chat leeway MCP server = %+v", got)
	}

	incAgentsMD, err := os.ReadFile(filepath.Join(root, "agents/sre-triage", IncidentBundle, ".agents", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	incMD := string(incAgentsMD)
	for _, want := range []string{
		"lookout_k8s_triage_workload",
		"lookout_k8s_resource_spec",
		"lookout_k8s_triage_logs",
		"lookout_k8s_event_timeline",
		"lookout_k8s_list_resources",
		"lookout_k8s_state_edges",
		"lookout_k8s_triage_delta",
		"overflow section=",
		"omit `namespace`",
		"unavailable in this runtime",
		"references/playbooks/",
		"projects/<project>/locations/<zone or region>/clusters/<cluster>",
	} {
		if !strings.Contains(incMD, want) {
			t.Errorf("gke-triage-f/.agents/AGENTS.md lacks %q", want)
		}
	}
	for _, unwanted := range []string{"gke_get_k8s_resource", "gke_describe_k8s_resource", "gke_get_k8s_logs", "gke_list_k8s_events", "finish_task` with", "finding.json", "_fallback", "lookout_k8s_audit_all"} {
		if strings.Contains(incMD, unwanted) {
			t.Errorf("gke-triage-f/.agents/AGENTS.md must not reference %q", unwanted)
		}
	}

	chatAgentsMD, err := os.ReadFile(filepath.Join(root, "agents/sre-triage", ChatBundle, ".agents", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	md := string(chatAgentsMD)
	for _, want := range []string{
		"lookout_k8s_list_clusters",
		"lookout_k8s_audit_all",
		"lookout_k8s_triage_workload",
		"lookout_k8s_resource_spec",
		"lookout_k8s_triage_logs",
		"lookout_k8s_event_timeline",
		"lookout_k8s_list_resources",
		"lookout_k8s_state_edges",
		"lookout_k8s_triage_delta",
		"leeway_report",
		"platform_managed",
		"projects/<project>/locations/<location>/clusters/<cluster>",
		"Always pass `clusters` to `leeway_report`",
		"never `cluster`",
		"overflow section=",
		"omit `namespace`",
		"unavailable in this runtime",
		"references/playbooks/",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("gke-chat/.agents/AGENTS.md lacks %q", want)
		}
	}
	for _, unwanted := range []string{"gke_list_clusters", "gke_get_cluster", "gke_get_k8s_resource", "gke_describe_k8s_resource", "gke_get_k8s_logs", "gke_list_k8s_events", "finish_task", "finding.json", "_fallback", "posture-audit", "v0.44.0", "all_namespaces: true", "requires an explicit namespace scope"} {
		if strings.Contains(md, unwanted) {
			t.Errorf("gke-chat/.agents/AGENTS.md must not reference %q", unwanted)
		}
	}

	chatCfgBytes, err := os.ReadFile(filepath.Join(root, "agents/sre-triage", ChatBundle, ".agents", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var chatCfg struct {
		Agent struct {
			MaxSteps            int     `json:"max_steps"`
			MaxWallclockSeconds int     `json:"max_wallclock_seconds"`
			MaxSessionCostUSD   float64 `json:"max_session_cost_usd"`
			FinalReport         bool    `json:"final_report"`
		} `json:"agent"`
		Permissions struct {
			Mode  string   `json:"mode"`
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(chatCfgBytes, &chatCfg); err != nil {
		t.Fatal(err)
	}
	if chatCfg.Agent.MaxSteps != 50 || chatCfg.Agent.MaxWallclockSeconds != 600 || chatCfg.Agent.MaxSessionCostUSD != 10 || !chatCfg.Agent.FinalReport {
		t.Errorf("gke-chat agent budgets = %+v", chatCfg.Agent)
	}
	wantAllow := []string{
		"mcp:lookout_k8s_list_clusters*",
		"mcp:lookout_k8s_audit_all*",
		"mcp:lookout_k8s_triage_workload*",
		"mcp:lookout_k8s_resource_spec*",
		"mcp:lookout_k8s_triage_logs*",
		"mcp:lookout_k8s_event_timeline*",
		"mcp:lookout_k8s_list_resources*",
		"mcp:lookout_k8s_state_edges*",
		"mcp:lookout_k8s_triage_delta*",
		"mcp:leeway_report*",
		"skill:list_skills*",
		"skill:load_skill*",
		"skill:load_skill_resource*",
	}
	if chatCfg.Permissions.Mode != "ask" || !slices.Equal(chatCfg.Permissions.Allow, wantAllow) {
		t.Errorf("gke-chat permissions = %+v, want ask with %q", chatCfg.Permissions, wantAllow)
	}
	for _, unwanted := range []string{"mcp:gke_", "spawn_agent:posture-audit", "spawn_agent:_fallback", "triage-classifier"} {
		if strings.Contains(string(chatCfgBytes), unwanted) {
			t.Errorf("gke-chat/.agents/config.json must not contain %q", unwanted)
		}
	}
}

// A chat task whose clusters span projects tells leeway mcp to sweep all of
// them; one whose clusters share the agent's project, and every incident
// task, carries no such setting.
func TestChatTaskPostureScope(t *testing.T) {
	core := func() *env.Env {
		e := testEnv()
		e.Agent.ChatImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("1", 64)
		e.Agent.IncidentImage = e.Agent.ChatImage
		return e
	}
	e := core()
	chat, err := ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chat), "_CLUSTERS_FROM") {
		t.Errorf("single-project chat task sets a cluster scope:\n%s", chat)
	}

	e = core()
	e.Agent.Clusters = append(e.Agent.Clusters,
		env.Cluster{Name: "zeta", Path: "projects/zeta-project/locations/us-east1/clusters/zeta"},
		env.Cluster{Name: "alpha", Path: "projects/alpha-project/locations/us-east1/clusters/alpha"},
		env.Cluster{Name: "alpha2", Path: "projects/alpha-project/locations/us-east4/clusters/alpha2"},
	)
	chat, err = ChatTask(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	want := "    - name: LEEWAY_CLUSTERS_FROM\n      value: \"example-project,alpha-project,zeta-project\"\n"
	if !strings.Contains(string(chat), want) {
		t.Errorf("multi-project chat task lacks %q:\n%s", want, chat)
	}
	// lookout runs --clusters-from=auto, which ignores this variable.
	if strings.Contains(string(chat), "name: LOOKOUT_CLUSTERS_FROM") {
		t.Error("chat task sets LOOKOUT_CLUSTERS_FROM, which --clusters-from=auto ignores")
	}
	inc, err := Task(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inc), "name: LEEWAY_CLUSTERS_FROM") {
		t.Errorf("incident task sets a posture scope (#131):\n%s", inc)
	}
}

func TestAuditConfigAndTask(t *testing.T) {
	e := testEnv()
	if cfg, err := AuditConfig(root, e); err != nil || cfg != nil {
		t.Fatalf("AuditConfig without audits: %s, %v", cfg, err)
	}
	if task, err := AuditTask(root, e, tok); err != nil || task != nil {
		t.Fatalf("AuditTask without audits: %s, %v", task, err)
	}

	e.Agent.ChatImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("1", 64)
	e.Agent.VCS = &env.VCS{
		Secret:       "gh-token",
		Repositories: []env.Repository{{Repository: "org/gitops"}},
	}
	e.Audits = &env.Audits{AutoPR: true}

	cfg, err := AuditConfig(root, e)
	if err != nil || len(cfg) == 0 {
		t.Fatalf("AuditConfig: %v", err)
	}
	for _, want := range []string{`"vcs_publish:*"`, `"vcs_propose:*"`, `"spawn_agent:propose-change"`, `"max_wallclock_seconds": 240`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("AuditConfig lacks %s:\n%s", want, cfg)
		}
	}

	task, err := AuditTask(root, e, tok)
	if err != nil || !strings.Contains(string(task), "/workspace/repo/audit-config.json") || !strings.Contains(string(task), "ate-credentials/gh-token/bearer") {
		t.Fatalf("AuditTask: %v\n%s", err, task)
	}

	ws, err := Workspace(root, e)
	if err != nil || !strings.Contains(string(ws), "path: audit-config.json") {
		t.Fatalf("Workspace lacks audit-config.json: %v\n%s", err, ws)
	}

	// IncidentImage fallback when ChatImage is unset, missing-image error, bad-root error, and ensureSubmap nil init.
	e.Agent.ChatImage = ""
	e.Agent.IncidentImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("2", 64)
	if task2, err := AuditTask(root, e, tok); err != nil || !strings.Contains(string(task2), e.Agent.IncidentImage) {
		t.Fatalf("AuditTask IncidentImage fallback: %v", err)
	}
	if _, err := AuditConfig(t.TempDir(), e); err == nil {
		t.Error("AuditConfig on empty root succeeded")
	}
	if _, err := Workspace(t.TempDir(), e); err == nil {
		t.Error("Workspace on empty root succeeded")
	}
	e.Agent.IncidentImage = ""
	if cfgNil, err := AuditConfig(root, e); err != nil || cfgNil != nil {
		t.Errorf("AuditConfig without images = %s, %v", cfgNil, err)
	}
	if _, err := AuditTask(root, e, tok); err == nil {
		t.Error("AuditTask without images succeeded")
	}
	emptyDoc := map[string]any{}
	ensureSubmap(emptyDoc, "agent")["x"] = 1
	if ensureSubmap(emptyDoc, "agent")["x"] != 1 {
		t.Error("ensureSubmap did not persist submap")
	}
}
