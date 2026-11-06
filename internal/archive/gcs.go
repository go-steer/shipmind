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

// Package archive stores a scenario run's record in Cloud Storage, so it
// can be read, compared and re-graded after the run (#48).
//
// It speaks the JSON API over plain HTTP with the token GKE's metadata
// server hands the pod's workload identity, so shipmind carries no
// Cloud client libraries. Every object is written with
// ifGenerationMatch=0: a run never overwrites one already there, so the
// writer needs only roles/storage.objectCreator and an archived run stays
// as it was written.
package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// GCS writes under gs://Bucket/Prefix.
type GCS struct {
	Bucket string
	Prefix string

	// Client defaults to one with a 30s timeout. TokenURL and APIBase
	// default to GKE's metadata server and storage.googleapis.com; tests
	// replace them.
	Client   *http.Client
	TokenURL string
	APIBase  string
}

const (
	// The metadata server's address, not a credential: it hands out the
	// pod's workload-identity token, read at Save time.
	defaultTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" // #nosec G101
	defaultAPIBase  = "https://storage.googleapis.com"
)

// Parse reads "gs://bucket/prefix" (the prefix may be empty or nested).
func Parse(s string) (*GCS, error) {
	rest, ok := strings.CutPrefix(s, "gs://")
	bucket, prefix, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || strings.ContainsAny(bucket, " \t\n") {
		return nil, fmt.Errorf("archive %q: want gs://bucket/prefix", s)
	}
	return &GCS{Bucket: bucket, Prefix: strings.Trim(prefix, "/")}, nil
}

func (g *GCS) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Save writes files under Prefix/dir, one object each. It stops at the
// first failure and says which object it was.
func (g *GCS) Save(ctx context.Context, dir string, files map[string][]byte) error {
	tok, err := g.token(ctx)
	if err != nil {
		return err
	}
	base := g.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	for name, data := range files {
		obj := path.Join(g.Prefix, dir, name)
		u := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&ifGenerationMatch=0&name=%s",
			strings.TrimSuffix(base, "/"), url.PathEscape(g.Bucket), url.QueryEscape(obj))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", contentType(name))
		resp, err := g.client().Do(req)
		if err != nil {
			return fmt.Errorf("writing gs://%s/%s: %w", g.Bucket, obj, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("writing gs://%s/%s: %s: %s", g.Bucket, obj, resp.Status, strings.TrimSpace(string(body)))
		}
	}
	return nil
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".json":
		return "application/json"
	case ".sse":
		return "text/event-stream"
	}
	return "application/octet-stream"
}

func (g *GCS) token(ctx context.Context) (string, error) {
	u := g.TokenURL
	if u == "" {
		u = defaultTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := g.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("archive token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("archive token: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("archive token: no access_token in the metadata server's answer (%v)", err)
	}
	return t.AccessToken, nil
}
