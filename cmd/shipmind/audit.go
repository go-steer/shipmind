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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-steer/shipmind/internal/audit"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/deploy"
	"github.com/go-steer/shipmind/internal/scenario"
)

const auditUsage = `usage: shipmind audit run -config FILE -ledger FILE (-in-cluster | -kubeconfig FILE [-context C] | -context C) [-router-cert F -router-trust F]`

var (
	newAuditKube             = func(kubeContext string) scenarioKube { return cluster.Kube{Context: kubeContext} }
	writeInClusterKubeconfig = cluster.InClusterKubeconfig
	auditExec                = audit.ExecRunner
)

func runAuditCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, auditUsage)
		return 2
	}
	fs := flag.NewFlagSet("shipmind audit run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to rendered audit config.json")
	ledgerPath := fs.String("ledger", "", "path to persistent ledger.json")
	inCluster := fs.Bool("in-cluster", false, "run inside the cluster using the pod's service account")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (outside the cluster)")
	kubeContext := fs.String("context", "", "kubeconfig context (outside the cluster)")
	router := fs.String("router", "", "Substrate router URL (default in-cluster: https://atenet-router.ate-system.svc.cluster.local)")
	axServer := fs.String("ax-server", "", "ax-server address (default in-cluster: ax-server.ax-system.svc.cluster.local:8080)")
	routerCert := fs.String("router-cert", "", "podidentity mTLS client bundle")
	routerTrust := fs.String("router-trust", "", "servicedns mTLS trust bundle")
	routerName := fs.String("router-server-name", "", "router server name override")
	lookoutBin := fs.String("lookout-bin", "lookout", "lookout binary")
	leewayBin := fs.String("leeway-bin", "leeway", "leeway binary")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *configPath == "" || *ledgerPath == "" || (*inCluster == (*kubeContext != "" || *kubeconfig != "")) {
		fmt.Fprintln(stderr, auditUsage)
		return 2
	}
	useTLS := *routerCert != "" || *routerTrust != ""
	if useTLS && (*routerCert == "" || *routerTrust == "") {
		fmt.Fprintln(stderr, "shipmind audit run: router mTLS wants both -router-cert and -router-trust")
		return 2
	}

	cfg, err := audit.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind audit run: %v\n", err)
		return 1
	}
	if !*inCluster && *kubeContext != "" && !cfg.SingleStaticCluster() {
		fmt.Fprintln(stderr, "shipmind audit run: -context can only be used with a single static cluster; omit -context to select per-cluster contexts from -kubeconfig")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	opts := audit.RunOptions{
		LedgerPath: *ledgerPath,
		LookoutBin: *lookoutBin,
		LeewayBin:  *leewayBin,
		Exec:       auditExec,
		Log: func(format string, a ...any) {
			fmt.Fprintf(stderr, "audit: "+format+"\n", a...)
		},
	}

	secretDir := filepath.Dir(filepath.Clean(*configPath))
	if tokBytes, rerr := os.ReadFile(filepath.Join(secretDir, "ingress-token")); rerr == nil {
		opts.IngressToken = strings.TrimSpace(string(tokBytes))
	}

	if *inCluster {
		closeTLS, err := setupInClusterAudit(cfg, &opts, secretDir, *router, *axServer, *routerCert, *routerTrust, *routerName, useTLS)
		if err != nil {
			fmt.Fprintf(stderr, "shipmind audit run: %v\n", err)
			return 1
		}
		defer closeTLS()
	} else {
		opts.Kubeconfig = *kubeconfig
		opts.KubeContext = *kubeContext
	}

	summary, runErr := audit.Run(ctx, cfg, opts)
	if summary != nil {
		if b, merr := json.MarshalIndent(summary, "", "  "); merr == nil {
			fmt.Fprintln(stdout, string(b))
		}
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "shipmind audit run: %v\n", runErr)
		return 1
	}
	return 0
}

func setupInClusterAudit(
	cfg *audit.Config,
	opts *audit.RunOptions,
	secretDir, routerURL, axServer, routerCert, routerTrust, routerName string,
	useTLS bool,
) (func(), error) {
	noop := func() {}
	kc, err := writeInClusterKubeconfig(os.TempDir())
	if err != nil {
		return noop, err
	}
	if err := os.Setenv("KUBECONFIG", kc); err != nil {
		return noop, err
	}
	kube := newAuditKube(cluster.InClusterContext)
	opts.GoogleToken = cachedSecretTokenProvider(kube)

	if !cfg.AutoPR {
		return noop, nil
	}
	taskBytes, err := os.ReadFile(filepath.Join(secretDir, "task.yaml"))
	if err != nil {
		return noop, fmt.Errorf("auto_pr task.yaml: %w", err)
	}
	attachBytes, err := os.ReadFile(filepath.Join(secretDir, "token"))
	if err != nil || strings.TrimSpace(string(attachBytes)) == "" {
		return noop, errors.New("auto_pr attach token missing")
	}
	opts.TaskTemplate = taskBytes

	if axServer == "" {
		axServer = "ax-server.ax-system.svc.cluster.local:8080"
	}
	if routerURL == "" {
		routerURL = inClusterRouter
		if useTLS {
			routerURL = inClusterRouterTLS
		}
	}
	ax := scenario.Ax{Server: axServer, Atespace: cfg.Atespace}
	if bin := os.Getenv("SHIPMIND_AX"); bin != "" {
		ax.Bin = bin
	}
	rt := &scenario.Router{Base: routerURL, Atespace: cfg.Atespace, Token: strings.TrimSpace(string(attachBytes))}
	closeFn := noop
	if useTLS {
		c, closeTLS, err := newRouterClient(scenario.RouterTLS{
			CredentialBundle: routerCert,
			TrustBundle:      routerTrust,
			ServerName:       routerName,
		})
		if err != nil {
			return noop, err
		}
		rt.Client = c
		closeFn = func() { _ = closeTLS() }
	}
	opts.ChatTasks = newChatTasks(ax)
	opts.ChatAgent = newChatAgent(rt)
	return closeFn, nil
}

func cachedSecretTokenProvider(kube scenarioKube) audit.TokenProvider {
	var (
		mu      sync.Mutex
		cached  string
		fetched time.Time
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		if cached != "" && time.Since(fetched) < 2*time.Minute {
			tok := cached
			mu.Unlock()
			return tok, nil
		}
		mu.Unlock()

		data, ok, err := kube.Secret(ctx, deploy.CredentialsNamespace, "google-token")
		if err == nil && ok && len(data["token"]) > 0 {
			tok := strings.TrimSpace(string(data["token"]))
			mu.Lock()
			cached, fetched = tok, time.Now()
			mu.Unlock()
			return tok, nil
		}
		return metadataAccessToken(ctx)
	}
}

var metadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" //nolint:gosec // GCE metadata server endpoint URL, not a credential

func metadataAccessToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataTokenURL, nil) //nolint:gosec // fixed GCE metadata URL (overridable only in unit tests)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req) //nolint:gosec // fixed GCE metadata URL (overridable only in unit tests)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server token: status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		return "", errors.New("metadata server returned empty access_token")
	}
	return body.AccessToken, nil
}
