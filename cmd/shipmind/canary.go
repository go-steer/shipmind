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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-steer/shipmind/internal/bom"
	"github.com/go-steer/shipmind/internal/deploy"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/scenario"
)

// chatRun reads what a chat scenario's run is given on the command line,
// or nil for an incident scenario, which takes none of it.
func chatRun(s *scenario.Scenario, taskFile, workspaceFile, clusterPath string) (*scenario.ChatRun, error) {
	if s.Chat == nil {
		if taskFile != "" || workspaceFile != "" || clusterPath != "" {
			return nil, fmt.Errorf("-task, -workspace and -cluster are for a chat scenario; %s is not one", s.Name)
		}
		return nil, nil
	}
	if taskFile == "" || clusterPath == "" {
		return nil, fmt.Errorf("%s is a chat scenario: it wants -task and -cluster (`shipmind scenario canary` supplies them)", s.Name)
	}
	run := &scenario.ChatRun{Cluster: clusterPath}
	var err error
	if run.Task, err = os.ReadFile(filepath.Clean(taskFile)); err != nil {
		return nil, err
	}
	if workspaceFile != "" {
		if run.Workspace, err = os.ReadFile(filepath.Clean(workspaceFile)); err != nil {
			return nil, err
		}
	}
	return run, nil
}

// runCanary implements `shipmind scenario canary`: launch a chat scenario
// against a candidate agent image in a running environment, as a one-off
// Job, without changing anything the environment deploys (#127).
func runCanary(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shipmind scenario canary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "the shipmind checkout the candidate is built from: its task and Workspace templates, its scenarios")
	image := fs.String("image", "", "the candidate agent image (images/agent), pinned by digest")
	runner := fs.String("runner-image", "", "the e2e-runner image to run the scenario with, pinned by digest (default: versions.yaml's e2e-runner pin, which must be built from a commit that has the scenario)")
	clusterName := fs.String("cluster", "", "which of the environment's agent.clusters to ask about (default: the first)")
	dryRun := fs.Bool("dry-run", false, "render the Job and print it; apply nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 2 || *image == "" {
		fmt.Fprintln(stderr, "shipmind scenario canary: want -image, an environment directory and a scenario name")
		return 2
	}
	dir, name := fs.Arg(0), fs.Arg(1)
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		fmt.Fprintf(stderr, "shipmind scenario canary: scenario %q: want a scenario's name, a directory under e2e/scenarios\n", name)
		return 2
	}
	s, err := scenario.Load(filepath.Join(*root, "e2e", "scenarios", name))
	if err != nil {
		fmt.Fprintf(stderr, "shipmind scenario canary: %v\n", err)
		return 1
	}
	if s.Chat == nil {
		fmt.Fprintf(stderr, "shipmind scenario canary: %s is not a chat scenario; a canary asks the agent a question\n", name)
		return 2
	}
	if *runner == "" {
		if *runner, err = pinnedRunner(*root); err != nil {
			fmt.Fprintf(stderr, "shipmind scenario canary: %v\n", err)
			return 1
		}
	}
	e, err := env.Load(dir)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind scenario canary: %v\n", err)
		return 1
	}
	run, err := deploy.Canary(context.Background(), *root, e, newCluster(e.Context), deploy.CanaryOptions{
		Image: *image, RunnerImage: *runner, Scenario: name, Cluster: *clusterName, DryRun: *dryRun,
	})
	if run == nil {
		fmt.Fprintf(stderr, "shipmind scenario canary: %v\n", err)
		return 1
	}
	if *dryRun {
		fmt.Fprintf(stdout, "%s", run.Job)
		fmt.Fprintf(stderr, "dry run: %s would ask about %s; nothing applied\n", run.Name, run.Cluster)
		return 0
	}
	fmt.Fprintf(stdout, "%s: started in context %s, asking about %s\n", run.Name, e.Context, run.Cluster)
	fmt.Fprintf(stdout, "  follow:  kubectl --context %s -n %s logs -f job/%s\n", e.Context, deploy.E2ENamespace, run.Name)
	fmt.Fprintf(stdout, "  result:  kubectl --context %s -n %s get job/%s   (the log's last lines are the result, as JSON)\n", e.Context, deploy.E2ENamespace, run.Name)
	fmt.Fprintf(stdout, "  remove:  kubectl --context %s -n %s delete job,secret -l %s=%s\n", e.Context, deploy.E2ENamespace, deploy.CanaryLabel, run.Name)
	fmt.Fprintf(stdout, "           (the run deletes its own task and workspace; if its pod was killed first, `ax --context %s -a %s delete task %s`, then the same for `workspace`)\n", e.Context, e.Agent.Atespace, run.Name)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind scenario canary: %v\n", err)
		return 1
	}
	return 0
}

// pinnedRunner is versions.yaml's e2e-runner image.
func pinnedRunner(root string) (string, error) {
	f, err := bom.Load(filepath.Join(root, "versions.yaml"))
	if err != nil {
		return "", err
	}
	c, ok := f.Component("e2e-runner")
	if !ok || len(c.Images) == 0 {
		return "", errors.New("versions.yaml pins no e2e-runner image: pass -runner-image")
	}
	return c.Images[0].Ref, nil
}
