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

// Package env reads an environment's shipmind.yaml: the values a
// deployment needs that kustomize can't carry, next to the overlay's
// kustomization.yaml.
//
// An environment is an overlay directory holding a kustomization.yaml
// (which references a shipmind profile at a full commit SHA) and a
// shipmind.yaml (this package): the kube context every command runs
// against, the agent's settings for the ax task template and Workspace,
// the environment's Slack app names, and the changes `shipmind status`
// should expect.
package env

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// FileName is the environment file's name inside an overlay directory.
const FileName = "shipmind.yaml"

// Env is a parsed shipmind.yaml.
type Env struct {
	// Context is the kube context every command passes explicitly. The
	// current context is never used.
	Context string `yaml:"context"`
	// Agent configures the ax task template and Workspace.
	Agent Agent `yaml:"agent"`
	// Substrate, when set, says which Substrate build the cluster should
	// run; status checks its workloads against it.
	Substrate *Substrate `yaml:"substrate,omitempty"`
	// Slack, when set, names the environment's Slack app, which
	// `shipmind slack-app` prints switchboard's app manifest for.
	Slack *Slack `yaml:"slack,omitempty"`
	// Expect lists changes `shipmind status` reports as expected rather
	// than as drift, each with the reason: Kubernetes objects by the ID
	// kubectl diff gives them ("apps.v1.Deployment.ax-system.ax-redis"),
	// and the agent's objects as status names them ("secret
	// lookout-ax/lookout-ax-agent", "workspace <atespace>/<name>",
	// "deployment lookout-ax/lookout-ax", "substrate
	// DaemonSet/ate-system/atelet").
	Expect []Expected `yaml:"expect,omitempty"`

	// Dir is the overlay directory the file was read from.
	Dir string `yaml:"-"`
}

// Agent holds the values the agent's ax objects are rendered with.
type Agent struct {
	// Atespace is the ax atespace the Workspace and every task live in.
	Atespace string `yaml:"atespace"`
	// Project is the Google Cloud project the agent's Google calls bill to.
	Project string `yaml:"project"`
	// Workspace names the ax Workspace mounted into every task.
	Workspace string `yaml:"workspace"`
	// Image is the agent image, pinned by digest.
	Image string `yaml:"image"`
	// Clusters is the table the agent uses for incidents that name only
	// a bare cluster.
	Clusters []Cluster `yaml:"clusters"`
}

// Substrate describes the installed Substrate build. Substrate's
// installer applies it, not a render, so kubectl diff never sees it;
// status checks it against this instead.
type Substrate struct {
	// Images is the repository Substrate's own images come from, and Tag
	// the tag they all carry (Substrate's installer installs one tag).
	Images string `yaml:"images"`
	Tag    string `yaml:"tag"`
	// Digests pins each of Substrate's own images by name ("atelet"):
	// a tag can be republished, a digest can't.
	Digests map[string]string `yaml:"digests"`
	// Namespaces are where its workloads run.
	Namespaces []string `yaml:"namespaces"`
	// Workloads lists every workload those namespaces must hold, as
	// "<Kind>/<namespace>/<name>". A missing one, or one not listed, is
	// drift.
	Workloads []string `yaml:"workloads"`
}

// Slack names an environment's Slack app. Each environment gets its own
// app, so several can share one workspace.
type Slack struct {
	// App is the app's name and its bot's display name.
	App string `yaml:"app"`
	// Command is the app's slash command. switchboard ignores the
	// command word, but two apps in one workspace can't share it.
	Command string `yaml:"command"`
	// Description, when set, replaces switchboard's description of the
	// app.
	Description string `yaml:"description,omitempty"`
}

// Cluster maps a bare cluster name to its full GKE resource name.
type Cluster struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Expected is a change `shipmind status` should not count as drift.
type Expected struct {
	Object string `yaml:"object"`
	Reason string `yaml:"reason"`
}

var (
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	projectRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	clusterPath = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/locations/[a-z0-9-]+/clusters/[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	digestRef   = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	digestOnly  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	kubectlID   = regexp.MustCompile(`^[A-Za-z0-9.-]+\.[A-Za-z0-9]+\.[A-Za-z0-9-.]*\.[a-z0-9.-]+$`)
	slackApp    = regexp.MustCompile(`^[^\x00-\x1f]{1,35}$`)
	slashCmd    = regexp.MustCompile(`^/[a-z0-9][a-z0-9_-]{0,30}$`)
	agentID     = regexp.MustCompile(`^((secret|workspace|deployment) [a-z0-9-]+/[a-z0-9.-]+|(substrate|rollout) [A-Za-z]+/[a-z0-9-]+/[a-z0-9.-]+)$`)
)

// Load reads dir/shipmind.yaml and checks it.
func Load(dir string) (*Env, error) {
	p := filepath.Join(dir, FileName)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	e, err := Parse(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	e.Dir = dir
	return e, nil
}

// Parse decodes and checks one shipmind.yaml document.
func Parse(r io.Reader) (*Env, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var e Env
	if err := dec.Decode(&e); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty document")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one document")
	}
	if err := e.check(); err != nil {
		return nil, err
	}
	return &e, nil
}

func (e *Env) check() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if strings.TrimSpace(e.Context) == "" || strings.ContainsAny(e.Context, " \t\n") {
		fail("context %q: want a kube context name", e.Context)
	}
	a := e.Agent
	if !dnsLabel.MatchString(a.Atespace) {
		fail("agent.atespace %q: want a DNS label", a.Atespace)
	}
	if !projectRE.MatchString(a.Project) {
		fail("agent.project %q: want a Google Cloud project ID", a.Project)
	}
	if !dnsLabel.MatchString(a.Workspace) {
		fail("agent.workspace %q: want a DNS label", a.Workspace)
	}
	if !digestRef.MatchString(a.Image) {
		fail("agent.image %q: want an image pinned by digest", a.Image)
	}
	if len(a.Clusters) == 0 {
		fail("agent.clusters: want at least one cluster")
	}
	seen := map[string]bool{}
	for i, c := range a.Clusters {
		if !dnsLabel.MatchString(c.Name) {
			fail("agent.clusters[%d].name %q: want a GKE cluster name", i, c.Name)
		}
		if seen[c.Name] {
			fail("agent.clusters: %q listed twice", c.Name)
		}
		seen[c.Name] = true
		if !clusterPath.MatchString(c.Path) || !strings.HasSuffix(c.Path, "/clusters/"+c.Name) {
			fail("agent.clusters[%d].path %q: want projects/<project>/locations/<location>/clusters/%s", i, c.Path, c.Name)
		}
	}
	if sub := e.Substrate; sub != nil {
		last := sub.Images[strings.LastIndex(sub.Images, "/")+1:]
		if sub.Images == "" || !strings.Contains(sub.Images, "/") || strings.ContainsAny(sub.Images, "@ \t") || strings.Contains(last, ":") {
			fail("substrate.images %q: want an image repository (a registry port is fine) with no tag or digest", sub.Images)
		}
		if len(sub.Digests) == 0 {
			fail("substrate.digests: want each of Substrate's images pinned by digest")
		}
		for name, d := range sub.Digests {
			if !dnsLabel.MatchString(name) || !digestOnly.MatchString(d) {
				fail("substrate.digests: %q: %q, want <image name>: sha256:<64 hex>", name, d)
			}
		}
		if len(sub.Workloads) == 0 {
			fail("substrate.workloads: want the workloads Substrate's namespaces must hold")
		}
		for i, w := range sub.Workloads {
			parts := strings.Split(w, "/")
			if len(parts) != 3 || !slices.Contains([]string{"Deployment", "DaemonSet", "StatefulSet"}, parts[0]) ||
				!slices.Contains(sub.Namespaces, parts[1]) || !dnsLabel.MatchString(parts[2]) {
				fail("substrate.workloads[%d] %q: want Deployment|DaemonSet|StatefulSet/<one of substrate.namespaces>/<name>", i, w)
			}
		}
		if sub.Tag == "" || strings.ContainsAny(sub.Tag, "@/: \t") {
			fail("substrate.tag %q: want an image tag", sub.Tag)
		}
		if len(sub.Namespaces) == 0 {
			fail("substrate.namespaces: want at least one namespace")
		}
		for i, ns := range sub.Namespaces {
			if !dnsLabel.MatchString(ns) {
				fail("substrate.namespaces[%d] %q: want a namespace name", i, ns)
			}
		}
	}
	if sl := e.Slack; sl != nil {
		if !slackApp.MatchString(sl.App) || strings.TrimSpace(sl.App) != sl.App {
			fail("slack.app %q: want a Slack app name (1 to 35 characters, no surrounding spaces)", sl.App)
		}
		if !slashCmd.MatchString(sl.Command) {
			fail("slack.command %q: want a slash command (/ then up to 31 lowercase letters, digits, - or _)", sl.Command)
		}
		if len([]rune(sl.Description)) > 140 || strings.ContainsAny(sl.Description, "\n\r") {
			fail("slack.description: want one line of at most 140 characters")
		}
	}
	objs := map[string]bool{}
	for i, x := range e.Expect {
		if !kubectlID.MatchString(x.Object) && !agentID.MatchString(x.Object) {
			fail("expect[%d].object %q: want a kubectl diff object ID like apps.v1.Deployment.ns.name, or \"secret|workspace|deployment <namespace>/<name>\" or \"substrate|rollout <Kind>/<namespace>/<name>\"", i, x.Object)
		}
		if objs[x.Object] {
			fail("expect: %q listed twice", x.Object)
		}
		objs[x.Object] = true
		if strings.TrimSpace(x.Reason) == "" {
			fail("expect[%d] (%s): say why the change is expected", i, x.Object)
		}
	}
	return errors.Join(errs...)
}

// Expected reports whether a change to the object with this kubectl diff
// ID is expected, and why.
func (e *Env) Expected(id string) (string, bool) {
	for _, x := range e.Expect {
		if x.Object == id {
			return x.Reason, true
		}
	}
	return "", false
}
