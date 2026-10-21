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

package slackapp

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/env"
)

// The manifest switchboard ships, as this repository vendors it.
const vendored = "../../deploy/vendor/switchboard/deploy/slack/app-manifest.yaml"

func TestRenderVendored(t *testing.T) {
	in, err := os.ReadFile(vendored)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(in, env.Slack{App: "shipmind-stage", Command: "/shipmind-stage", Description: "Chat with the staging agents"}, "line one\n\nline three")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "# line one\n#\n# line three\ndisplay_information:\n") {
		t.Errorf("header:\n%s", out)
	}
	if strings.Contains(string(out), "switchboard") {
		t.Errorf("the old name survived:\n%s", out)
	}

	var got, want map[string]any
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(in, &want); err != nil {
		t.Fatal(err)
	}
	// Everything but the names is switchboard's.
	want["display_information"] = map[string]any{"name": "shipmind-stage", "description": "Chat with the staging agents"}
	features := want["features"].(map[string]any)
	features["bot_user"].(map[string]any)["display_name"] = "shipmind-stage"
	cmd := features["slash_commands"].([]any)[0].(map[string]any)
	cmd["command"] = "/shipmind-stage"
	cmd["description"] = strings.ReplaceAll(cmd["description"].(string), "switchboard", "shipmind-stage")
	if g, w := mustMarshal(t, got), mustMarshal(t, want); g != w {
		t.Errorf("got\n%s\nwant\n%s", g, w)
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRenderKeepsDescriptionAndQuotes(t *testing.T) {
	in := "display_information: {name: sb, description: keep}\nfeatures:\n  bot_user: {display_name: sb}\n  slash_commands: [{command: /sb}]\n"
	out, err := Render([]byte(in), env.Slack{App: "yes", Command: "/x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	// "yes" stays a string, not YAML 1.1's boolean.
	if m["display_information"]["name"] != "yes" || m["display_information"]["description"] != "keep" ||
		!strings.Contains(string(out), `"yes"`) {
		t.Errorf("output:\n%s", out)
	}
}

func TestRenderErrors(t *testing.T) {
	s := env.Slack{App: "a", Command: "/a"}
	for _, tc := range []struct{ name, in, want string }{
		{"not yaml", "a: [", "yaml"},
		{"not a mapping", "- a\n", "want a YAML mapping"},
		{"no name", "features: {}\n", "display_information.name"},
		{"empty name", "display_information: {name: ''}\n", "display_information.name: empty"},
		{"no bot", "display_information: {name: x}\n", "features.bot_user.display_name"},
		{"no commands", "display_information: {name: x}\nfeatures: {bot_user: {display_name: x}}\n", "exactly one slash command"},
		{"two commands", "display_information: {name: x}\nfeatures: {bot_user: {display_name: x}, slash_commands: [{command: /a}, {command: /b}]}\n", "exactly one slash command"},
		{"command not a string", "display_information: {name: x}\nfeatures: {bot_user: {display_name: x}, slash_commands: [{command: [x]}]}\n", "features.slash_commands[0]: command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Render([]byte(tc.in), s, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Render([]byte("display_information: {name: x}\nfeatures: {bot_user: {display_name: x}, slash_commands: [{command: /a}]}\n"),
		env.Slack{App: "a", Command: "/a", Description: "d"}, ""); err == nil || !strings.Contains(err.Error(), "display_information.description") {
		t.Errorf("description without one upstream: err = %v", err)
	}
}

func TestSet(t *testing.T) {
	for v, quoted := range map[string]bool{
		"yes": true, "No": true, "on": true, "OFF": true, "y": true, "~": true, "null": true, "": true,
		"12": true, "1.5": true, "-3": true, "1e3": true, "0x1F": true, "1:30": true, ".inf": true,
		"shipmind-stage": false, "/shipmind": false, "yesterday": false, "v1.2": false, "Chat with agents": false,
	} {
		var n yaml.Node
		set(&n, v)
		if got := n.Style == yaml.DoubleQuotedStyle; got != quoted {
			t.Errorf("set(%q): quoted = %v, want %v", v, got, quoted)
		}
	}
}
