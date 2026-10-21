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

// Package slackapp renders an environment's Slack app manifest from the
// one switchboard ships (deploy/slack/app-manifest.yaml, vendored at the
// version shipmind pins): the same scopes, events and settings, under
// the environment's names, so each environment gets its own app and
// several can share one workspace.
package slackapp

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/env"
)

// Render returns manifest with the app, its bot and its one slash
// command renamed after s, and s.Description in place of the app's
// description when set. The app's old name is replaced with the new one
// in the slash command's description and usage hint too, where Slack
// shows it. Comments are dropped (they explain switchboard's choices,
// and the copy they belong to is vendored); header, if not empty, is
// written first as a comment.
func Render(manifest []byte, s env.Slack, header string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a Slack app manifest: want a YAML mapping")
	}
	root := doc.Content[0]

	name, err := scalar(root, "display_information", "name")
	if err != nil {
		return nil, err
	}
	if name.Value == "" {
		return nil, errors.New("display_information.name: empty")
	}
	old := name.Value
	set(name, s.App)
	if s.Description != "" {
		desc, err := scalar(root, "display_information", "description")
		if err != nil {
			return nil, err
		}
		set(desc, s.Description)
	}
	bot, err := scalar(root, "features", "bot_user", "display_name")
	if err != nil {
		return nil, err
	}
	set(bot, s.App)

	cmds := lookup(root, "features", "slash_commands")
	if cmds == nil || cmds.Kind != yaml.SequenceNode || len(cmds.Content) != 1 || cmds.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("features.slash_commands: want exactly one slash command to rename")
	}
	cmd := cmds.Content[0]
	c, err := scalar(cmd, "command")
	if err != nil {
		return nil, fmt.Errorf("features.slash_commands[0]: %w", err)
	}
	set(c, s.Command)
	for _, key := range []string{"description", "usage_hint"} {
		if n := lookup(cmd, key); n != nil && n.Kind == yaml.ScalarNode {
			set(n, strings.ReplaceAll(n.Value, old, s.App))
		}
	}

	stripComments(&doc)
	var buf bytes.Buffer
	if header != "" {
		for line := range strings.SplitSeq(strings.TrimRight(header, "\n"), "\n") {
			fmt.Fprintln(&buf, strings.TrimRight("# "+line, " "))
		}
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// lookup follows keys through nested mappings from n; nil if any is
// missing.
func lookup(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		if n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// scalar is lookup for a string the manifest must have.
func scalar(n *yaml.Node, keys ...string) (*yaml.Node, error) {
	v := lookup(n, keys...)
	if v == nil || v.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("%s: want a string", strings.Join(keys, "."))
	}
	return v, nil
}

// yaml11 matches plain scalars a YAML 1.1 parser reads as something
// other than a string: booleans, null, numbers. yaml.v3 writes them
// unquoted, since YAML 1.2 reads most of them as strings.
var yaml11 = regexp.MustCompile(`^(?i:y|n|yes|no|on|off|true|false|null|~|[-+]?(\.?[0-9][0-9_.:]*([eE][-+]?[0-9]+)?|\.inf|\.nan|0x[0-9a-f_]+|0b[01_]+))$`)

// set gives a string node a new value, quoted when a YAML 1.1 reader
// would take it for another type.
func set(v *yaml.Node, s string) {
	v.Value, v.Tag, v.Style = s, "!!str", 0
	if s == "" || yaml11.MatchString(s) {
		v.Style = yaml.DoubleQuotedStyle
	}
}

func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}
