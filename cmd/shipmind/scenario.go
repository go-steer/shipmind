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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"cloud.google.com/go/compute/metadata"

	"github.com/go-steer/shipmind/internal/archive"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/deploy"
	"github.com/go-steer/shipmind/internal/judge"
	"github.com/go-steer/shipmind/internal/scenario"
)

// inClusterRouter is Substrate's router as seen from inside the cluster,
// over plain HTTP (Substrate v0.5) or mTLS (v1.0).
const (
	inClusterRouter    = "http://atenet-router.ate-system.svc.cluster.local"
	inClusterRouterTLS = "https://atenet-router.ate-system.svc.cluster.local"
)

// scenarioKube is what a run needs from kubectl.
type scenarioKube interface {
	scenario.Kube
	Secret(ctx context.Context, namespace, name string) (map[string][]byte, bool, error)
}

// What a run touches; tests replace these.
var (
	newScenarioKube     = func(kubeContext string) scenarioKube { return cluster.Kube{Context: kubeContext} }
	newScenarioTasks    = func(ax scenario.Ax) scenario.Tasks { return ax }
	newScenarioAgent    = func(r *scenario.Router) scenario.Agent { return r }
	newScenarioArchiver = func(g *archive.GCS) scenario.Archiver { return g }
	newRouterClient     = func(t scenario.RouterTLS) (*http.Client, func() error, error) { return t.Client() }
	newJudge            = func(ctx context.Context, model, project, region, effort string) (scenario.Grader, error) {
		return judge.NewVertex(ctx, model, project, region, effort)
	}
	// metadataProject is the project of the Google Cloud machine this runs
	// on (a GKE pod's, under workload identity), or "" off Google Cloud.
	metadataProject = func(ctx context.Context) string {
		if !metadata.OnGCE() {
			return ""
		}
		p, err := metadata.ProjectIDWithContext(ctx)
		if err != nil {
			return ""
		}
		return p
	}
)

const scenarioUsage = `usage: shipmind scenario run -atespace A (-context C -router URL | -in-cluster) [-router-cert F -router-trust F] [-keep] [-json FILE] [-archive gs://bucket/prefix] [-judge MODEL -judge-project P] <scenario-dir>
       shipmind scenario judge -model MODEL -project P [-region R] [-effort E] [-scenarios DIR] <result.json>...`

// judgeFlags are the judge model's flags, shared by run and judge.
type judgeFlags struct{ model, project, region, effort *string }

func addJudgeFlags(fs *flag.FlagSet, prefix string) judgeFlags {
	name := func(n string) string {
		if prefix == "" {
			return n
		}
		return prefix + "-" + n
	}
	model := "model"
	if prefix != "" {
		model = prefix
	}
	return judgeFlags{
		model:   fs.String(model, "", "judge model on Vertex AI, e.g. claude-opus-5-5 (#90)"),
		project: fs.String(name("project"), os.Getenv("GOOGLE_CLOUD_PROJECT"), "the judge's Google Cloud project (default $GOOGLE_CLOUD_PROJECT, else the machine's own, from the metadata server)"),
		region:  fs.String(name("region"), "global", "the judge's Vertex AI region"),
		effort:  fs.String(name("effort"), judge.DefaultEffort, "the judge's effort: low, medium, high, xhigh or max"),
	}
}

func (f judgeFlags) grader(ctx context.Context) (scenario.Grader, error) {
	if *f.model == "" {
		return nil, nil
	}
	project := *f.project
	if project == "" {
		project = metadataProject(ctx)
	}
	if project == "" {
		return nil, errors.New("a judge model wants its Google Cloud project: set it, or $GOOGLE_CLOUD_PROJECT, off Google Cloud")
	}
	return newJudge(ctx, *f.model, project, *f.region, *f.effort)
}

// runScenarioCmd implements `shipmind scenario`.
func runScenarioCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "judge" {
		return runJudge(args[1:], stdout, stderr)
	}
	return runScenario(args, stdout, stderr)
}

// runScenario implements `shipmind scenario run`.
func runScenario(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, scenarioUsage)
		return 2
	}
	fs := flag.NewFlagSet("shipmind scenario run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	atespace := fs.String("atespace", "", "the agents' ax atespace")
	kubeContext := fs.String("context", "", "kube context (from a workstation)")
	inCluster := fs.Bool("in-cluster", false, "run inside the cluster: the pod's service account, ax-server and the router by their service names")
	router := fs.String("router", "", "Substrate router URL (from a workstation: a port-forward to ate-system/atenet-router)")
	axServer := fs.String("ax-server", "", "ax-server address (default in-cluster: ax-server.ax-system.svc.cluster.local:8080)")
	routerCert := fs.String("router-cert", "", "for a router that requires mTLS: this pod's podidentity certificate (a podCertificate projection's credential bundle)")
	routerTrust := fs.String("router-trust", "", "for a router that requires mTLS: the router signer's trust bundle (a clusterTrustBundle projection)")
	routerName := fs.String("router-server-name", "", "with mTLS, the name to verify the router's certificate against (default "+scenario.RouterServerName+")")
	keep := fs.Bool("keep", false, "leave the workload and task in place for inspection")
	jsonOut := fs.String("json", "", "also write the result as JSON to this file")
	archiveTo := fs.String("archive", "", "store each run's record (the session's events, usage, result) under gs://bucket/prefix, with the pod's workload identity")
	jf := addJudgeFlags(fs, "judge")
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
	useTLS := *routerCert != "" || *routerTrust != ""
	if useTLS && (*routerCert == "" || *routerTrust == "") {
		fmt.Fprintln(stderr, "shipmind scenario run: router mTLS wants both -router-cert and -router-trust")
		return 2
	}
	if *routerName != "" && !useTLS {
		fmt.Fprintln(stderr, "shipmind scenario run: -router-server-name is for mTLS (-router-cert and -router-trust)")
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
			if useTLS {
				*router = inClusterRouterTLS
			}
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
	if useTLS {
		c, closeTLS, err := newRouterClient(scenario.RouterTLS{CredentialBundle: *routerCert, TrustBundle: *routerTrust, ServerName: *routerName})
		if err != nil {
			fmt.Fprintf(stderr, "shipmind scenario run: %v\n", err)
			return 1
		}
		defer func() { _ = closeTLS() }()
		rt.Client = c
	}

	var archiver scenario.Archiver
	if *archiveTo != "" {
		g, err := archive.Parse(*archiveTo)
		if err != nil {
			fmt.Fprintf(stderr, "shipmind scenario run: %v\n", err)
			return 2
		}
		archiver = newScenarioArchiver(g)
	}
	// A judge that can't be set up is recorded in the result, not a
	// reason to skip the run: the judge reports, it doesn't gate (#90).
	grader, err := jf.grader(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind scenario run: judge: %v\n", err)
		grader = judge.Broken{Model: *jf.model, Err: err}
	}
	res, runErr := scenario.Run(ctx, s, kube, newScenarioTasks(ax), newScenarioAgent(rt), scenario.Options{
		Keep:    *keep,
		Archive: archiver,
		Judge:   grader,
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

// graded is one archived run as `scenario judge` reads it: a result.json,
// or any JSON with the same three fields (a hand-written calibration
// answer). Want, if set, is the score band a calibration answer should
// get, such as "1-2", printed beside the grade.
type graded struct {
	Scenario string `json:"scenario"`
	Workload string `json:"workload"`
	Answer   string `json:"answer"`
	Want     string `json:"want,omitempty"`
}

// runJudge implements `shipmind scenario judge`: grade stored answers
// against their scenarios' diagnoses, for calibrating a judge and for
// re-grading archived runs (#91).
func runJudge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shipmind scenario judge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jf := addJudgeFlags(fs, "")
	scenarios := fs.String("scenarios", "e2e/scenarios", "the scenario directories, by name")
	jsonOut := fs.Bool("json", false, "print each grade as a JSON line")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 || *jf.model == "" {
		fmt.Fprintln(stderr, scenarioUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	grader, err := jf.grader(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind scenario judge: %v\n", err)
		return 2
	}
	loaded := map[string]*scenario.Scenario{}
	status, total := 0, 0.0
	for _, path := range fs.Args() {
		b, err := os.ReadFile(filepath.Clean(path))
		var in graded
		if err == nil {
			err = json.Unmarshal(b, &in)
		}
		if err == nil && (in.Scenario == "" || in.Workload == "") {
			err = errors.New("want scenario and workload fields")
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, err)
			status = 1
			continue
		}
		s, ok := loaded[in.Scenario]
		if !ok {
			if s, err = scenario.Load(filepath.Join(*scenarios, in.Scenario)); err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", path, err)
				status = 1
				continue
			}
			loaded[in.Scenario] = s
		}
		expected, err := s.Diagnosis(in.Workload)
		if err == nil && expected == "" {
			err = fmt.Errorf("scenario %s has no expect.diagnosis", in.Scenario)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, err)
			status = 1
			continue
		}
		g, err := grader.Grade(ctx, expected, in.Answer)
		total += g.CostUSD
		if err != nil {
			status = 1
		}
		if *jsonOut {
			b, _ := json.Marshal(struct {
				File string `json:"file"`
				Want string `json:"want,omitempty"`
				judge.Grade
			}{path, in.Want, g})
			fmt.Fprintln(stdout, string(b))
			continue
		}
		if err != nil {
			fmt.Fprintf(stdout, "%s: %v\n", path, err)
			continue
		}
		want := ""
		if in.Want != "" {
			want = " (want " + in.Want + ")"
		}
		fmt.Fprintf(stdout, "%s: %d/5%s correct=%v specific=%v actionable=%v $%.4f\n  %s\n",
			path, g.Score, want, g.CorrectDiagnosis, g.Specific, g.Actionable, g.CostUSD, g.Reasoning)
	}
	if !*jsonOut {
		fmt.Fprintf(stdout, "%d graded by %s at %s effort, $%.4f\n", fs.NArg(), *jf.model, *jf.effort, total)
	}
	return status
}
