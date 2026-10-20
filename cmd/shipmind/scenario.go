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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/deploy"
	"github.com/go-steer/shipmind/internal/scenario"
)

// inClusterRouter is Substrate's router as seen from inside the cluster.
const inClusterRouter = "http://atenet-router.ate-system.svc.cluster.local"

// scenarioKube is what a run needs from kubectl.
type scenarioKube interface {
	scenario.Kube
	Secret(ctx context.Context, namespace, name string) (map[string][]byte, bool, error)
}

// What a run touches; tests replace these.
var (
	newScenarioKube  = func(kubeContext string) scenarioKube { return cluster.Kube{Context: kubeContext} }
	newScenarioTasks = func(ax scenario.Ax) scenario.Tasks { return ax }
	newScenarioAgent = func(r *scenario.Router) scenario.Agent { return r }
)

// runScenario implements `shipmind scenario run`.
func runScenario(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, "usage: shipmind scenario run -atespace A (-context C -router URL | -in-cluster) [-keep] [-json FILE] <scenario-dir>")
		return 2
	}
	fs := flag.NewFlagSet("shipmind scenario run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	atespace := fs.String("atespace", "", "the agents' ax atespace")
	kubeContext := fs.String("context", "", "kube context (from a workstation)")
	inCluster := fs.Bool("in-cluster", false, "run inside the cluster: the pod's service account, ax-server and the router by their service names")
	router := fs.String("router", "", "Substrate router URL (from a workstation: a port-forward to ate-system/atenet-router)")
	axServer := fs.String("ax-server", "", "ax-server address (default in-cluster: ax-server.ax-system.svc.cluster.local:8080)")
	keep := fs.Bool("keep", false, "leave the workload and task in place for inspection")
	jsonOut := fs.String("json", "", "also write the result as JSON to this file")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || *atespace == "" || (*inCluster == (*kubeContext != "")) {
		fmt.Fprintln(stderr, "shipmind scenario run: want -atespace, exactly one of -context and -in-cluster, and one scenario directory")
		return 2
	}
	if *inCluster {
		path, err := cluster.InClusterKubeconfig(os.TempDir())
		if err != nil {
			fmt.Fprintf(stderr, "shipmind scenario run: %v\n", err)
			return 1
		}
		if err := os.Setenv("KUBECONFIG", path); err != nil {
			fmt.Fprintf(stderr, "shipmind scenario run: %v\n", err)
			return 1
		}
		*kubeContext = cluster.InClusterContext
		if *router == "" {
			*router = inClusterRouter
		}
		if *axServer == "" {
			*axServer = "ax-server.ax-system.svc.cluster.local:8080"
		}
	}
	if *router == "" {
		fmt.Fprintln(stderr, "shipmind scenario run: -router is required outside the cluster")
		return 2
	}

	s, err := scenario.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "shipmind scenario run: %v\n", err)
		return 1
	}
	// A SIGTERM (a Job's deadline, a node drain) cancels the run, and the
	// runner still cleans up: scenario.Run's cleanup outlives ctx.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ax := scenario.Ax{Server: *axServer, Atespace: *atespace}
	if *axServer == "" {
		ax.Context = *kubeContext
	}
	if bin := os.Getenv("SHIPMIND_AX"); bin != "" {
		ax.Bin = bin
	}
	kube := newScenarioKube(*kubeContext)
	data, ok, err := kube.Secret(ctx, deploy.Namespace, deploy.AgentSecret)
	if err != nil || !ok || len(data["token"]) == 0 {
		fmt.Fprintf(stderr, "shipmind scenario run: the agent's attach token (secret %s/%s, key token): found=%v %v\n", deploy.Namespace, deploy.AgentSecret, ok, err)
		return 1
	}
	rt := &scenario.Router{Base: *router, Atespace: *atespace, Token: string(data["token"])}

	res, runErr := scenario.Run(ctx, s, kube, newScenarioTasks(ax), newScenarioAgent(rt), scenario.Options{
		Keep: *keep,
		Log: func(format string, a ...any) {
			fmt.Fprintf(stderr, "scenario %s: "+format+"\n", append([]any{s.Name}, a...)...)
		},
	})
	if runErr != nil {
		fmt.Fprintf(stderr, "shipmind scenario run: %v\n", runErr)
		if res == nil {
			return 1
		}
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "shipmind scenario run: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	if *jsonOut != "" {
		if err := os.WriteFile(filepath.Clean(*jsonOut), append(b, '\n'), 0o600); err != nil {
			fmt.Fprintf(stderr, "shipmind scenario run: %v\n", err)
			return 1
		}
	}
	if !res.Pass {
		return 1
	}
	return 0
}
