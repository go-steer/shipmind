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

package audit

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenProvider returns a current Google Cloud OAuth2 access token.
type TokenProvider func(ctx context.Context) (string, error)

type clusterEndpoint struct {
	baseURL string
	client  *http.Client
}

// CredentialBroker serves a per-run loopback CREDENTIAL_PROXY_URL endpoint so
// subprocesses (`lookout audit` and `leeway report`) reach Google REST APIs
// and GKE control planes with the cluster's refreshed Google token without
// needing GCE instance metadata or cross-namespace Secret mounts.
type CredentialBroker struct {
	URL            string
	TokenFile      string
	KubeconfigPath string

	proxyToken  string
	googleToken TokenProvider
	gcpBaseURL  string // empty uses https://<host>; overridden in unit tests
	allowed     map[string]bool
	httpClient  *http.Client
	server      *http.Server

	mu        sync.Mutex
	endpoints map[string]*clusterEndpoint
}

// BrokerOptions configures StartCredentialBroker.
type BrokerOptions struct {
	Dir          string
	ClusterPaths []string
	GoogleToken  TokenProvider
	HTTPClient   *http.Client
	GCPBaseURL   string
}

// StartCredentialBroker starts a loopback HTTP credential proxy on 127.0.0.1:0
// and writes the proxy token file and GKE-context kubeconfig into opts.Dir.
func StartCredentialBroker(opts BrokerOptions) (*CredentialBroker, error) {
	if opts.GoogleToken == nil {
		return nil, errors.New("credential broker: GoogleToken provider is required")
	}
	rawTok := make([]byte, 32)
	if _, err := rand.Read(rawTok); err != nil {
		return nil, err
	}
	proxyTok := hex.EncodeToString(rawTok)

	tokPath := filepath.Join(opts.Dir, "proxy-token")
	if err := os.WriteFile(tokPath, []byte(proxyTok), 0o600); err != nil {
		return nil, err
	}

	allowed := make(map[string]bool, len(opts.ClusterPaths))
	for _, cp := range opts.ClusterPaths {
		p, l, c, ok := ParseGKEClusterPath(cp)
		if !ok {
			return nil, fmt.Errorf("credential broker: invalid GKE cluster path %q", cp)
		}
		allowed[p+"/"+l+"/"+c] = true
	}

	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout: 90 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	baseURL := "http://" + ln.Addr().String()

	kcPath := filepath.Join(opts.Dir, "kubeconfig")
	if err := writeBrokerKubeconfig(kcPath, baseURL, tokPath, opts.ClusterPaths); err != nil {
		_ = ln.Close()
		return nil, err
	}

	b := &CredentialBroker{
		URL:            baseURL,
		TokenFile:      tokPath,
		KubeconfigPath: kcPath,
		proxyToken:     proxyTok,
		googleToken:    opts.GoogleToken,
		gcpBaseURL:     strings.TrimRight(opts.GCPBaseURL, "/"),
		allowed:        allowed,
		httpClient:     hc,
		endpoints:      map[string]*clusterEndpoint{},
	}
	b.server = &http.Server{
		Handler:           http.HandlerFunc(b.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = b.server.Serve(ln) }()
	return b, nil
}

// Close shuts down the loopback credential proxy.
func (b *CredentialBroker) Close() error {
	if b == nil || b.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return b.server.Shutdown(ctx)
}

// ContextName returns the GKE kubeconfig context name for path.
func ContextName(path string) string {
	p, l, c, ok := ParseGKEClusterPath(path)
	if !ok {
		return path
	}
	return fmt.Sprintf("gke_%s_%s_%s", p, l, c)
}

func writeBrokerKubeconfig(path, baseURL, tokFile string, clusterPaths []string) error {
	var sb strings.Builder
	sb.WriteString("apiVersion: v1\nkind: Config\nclusters:\n")
	for _, cp := range clusterPaths {
		p, l, c, _ := ParseGKEClusterPath(cp)
		ctxName := fmt.Sprintf("gke_%s_%s_%s", p, l, c)
		fmt.Fprintf(&sb, "  - name: %s\n    cluster:\n      server: %s/v1/k8s/%s/%s/%s\n", ctxName, baseURL, p, l, c)
	}
	fmt.Fprintf(&sb, "users:\n  - name: broker\n    user:\n      tokenFile: %s\ncontexts:\n", tokFile)
	first := ""
	for _, cp := range clusterPaths {
		ctxName := ContextName(cp)
		if first == "" {
			first = ctxName
		}
		fmt.Fprintf(&sb, "  - name: %s\n    context:\n      cluster: %s\n      user: broker\n", ctxName, ctxName)
	}
	if first != "" {
		fmt.Fprintf(&sb, "current-context: %s\n", first)
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

func (b *CredentialBroker) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !b.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tok, err := b.googleToken(r.Context())
	if err != nil || strings.TrimSpace(tok) == "" {
		http.Error(w, "google token unavailable", http.StatusBadGateway)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/gcp/"):
		b.proxyGCP(w, r, strings.TrimSpace(tok))
	case strings.HasPrefix(r.URL.Path, "/v1/k8s/"):
		b.proxyK8s(w, r, strings.TrimSpace(tok))
	default:
		http.NotFound(w, r)
	}
}

func (b *CredentialBroker) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := []byte(strings.TrimSpace(h[len(prefix):]))
	want := []byte(b.proxyToken)
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func (b *CredentialBroker) proxyGCP(w http.ResponseWriter, r *http.Request, tok string) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/gcp/")
	host, tail, ok := strings.Cut(rest, "/")
	if !ok || !validGoogleHost(host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	targetBase := "https://" + host
	if b.gcpBaseURL != "" {
		targetBase = b.gcpBaseURL
	}
	targetURL := targetBase + "/" + tail
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	forwardRequest(w, r, b.httpClient, targetURL, tok)
}

func validGoogleHost(host string) bool {
	return strings.HasSuffix(host, ".googleapis.com") &&
		!strings.ContainsAny(host, "/:\\@ \t") &&
		!strings.Contains(host, "..")
}

func (b *CredentialBroker) proxyK8s(w http.ResponseWriter, r *http.Request, tok string) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/k8s/")
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	triple := parts[0] + "/" + parts[1] + "/" + parts[2]
	if !b.allowed[triple] {
		http.Error(w, "cluster not in audit scope", http.StatusForbidden)
		return
	}
	ep, err := b.resolveClusterEndpoint(r.Context(), parts[0], parts[1], parts[2], tok)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	tail := ""
	if len(parts) == 4 {
		tail = parts[3]
	}
	targetURL := ep.baseURL + "/" + tail
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	forwardRequest(w, r, ep.client, targetURL, tok)
}

func (b *CredentialBroker) resolveClusterEndpoint(ctx context.Context, project, location, cluster, tok string) (*clusterEndpoint, error) {
	triple := project + "/" + location + "/" + cluster
	b.mu.Lock()
	cached := b.endpoints[triple]
	b.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	apiBase := "https://container.googleapis.com"
	if b.gcpBaseURL != "" {
		apiBase = b.gcpBaseURL
	}
	u := fmt.Sprintf("%s/v1/projects/%s/locations/%s/clusters/%s", apiBase, project, location, cluster)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil) //nolint:gosec // project/location/cluster matched against allowlisted GKE cluster paths
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := b.httpClient.Do(req) //nolint:gosec // target is container.googleapis.com (or test server) for allowlisted GKE cluster
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clusters.get %s: status %d", triple, resp.StatusCode)
	}
	ep, err := parseClusterEndpoint(body, b.httpClient)
	if err != nil {
		return nil, fmt.Errorf("clusters.get %s: %w", triple, err)
	}
	b.mu.Lock()
	b.endpoints[triple] = ep
	b.mu.Unlock()
	return ep, nil
}

func parseClusterEndpoint(body []byte, defaultClient *http.Client) (*clusterEndpoint, error) {
	var rec struct {
		Endpoint                    string `json:"endpoint"`
		ControlPlaneEndpointsConfig struct {
			DNSEndpointConfig struct {
				Endpoint string `json:"endpoint"`
			} `json:"dnsEndpointConfig"`
		} `json:"controlPlaneEndpointsConfig"`
		MasterAuth struct {
			ClusterCaCertificate string `json:"clusterCaCertificate"`
		} `json:"masterAuth"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, err
	}
	if dns := strings.TrimSpace(rec.ControlPlaneEndpointsConfig.DNSEndpointConfig.Endpoint); dns != "" {
		if !strings.HasPrefix(dns, "https://") && !strings.HasPrefix(dns, "http://") {
			dns = "https://" + dns
		}
		return &clusterEndpoint{baseURL: strings.TrimRight(dns, "/"), client: defaultClient}, nil
	}
	ip := strings.TrimSpace(rec.Endpoint)
	if ip == "" {
		return nil, errors.New("cluster record carries neither dnsEndpoint nor endpoint")
	}
	base := ip
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		base = "https://" + base
	}
	client := defaultClient
	if strings.HasPrefix(base, "https://") && rec.MasterAuth.ClusterCaCertificate != "" {
		pemBytes, err := base64.StdEncoding.DecodeString(rec.MasterAuth.ClusterCaCertificate)
		if err != nil {
			return nil, fmt.Errorf("clusterCaCertificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("clusterCaCertificate contained no valid PEM certificates")
		}
		client = &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &clusterEndpoint{baseURL: strings.TrimRight(base, "/"), client: client}, nil
}

func forwardRequest(w http.ResponseWriter, r *http.Request, client *http.Client, targetURL, tok string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body) //nolint:gosec // targetURL is validated against *.googleapis.com or clusters.get endpoint
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maps.Copy(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := client.Do(req) //nolint:gosec // targetURL is validated against *.googleapis.com or clusters.get endpoint
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
