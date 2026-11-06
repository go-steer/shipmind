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
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
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

// Save writes files under Prefix/dir, one object each, in name order with
// meta.json last, so a run whose meta.json is there is complete. A write
// that fails transiently (429, 5xx, the connection) is retried; an object
// a retry finds already there (412) was written by the attempt before.
// Save stops at the first write that still fails and says which.
func (g *GCS) Save(ctx context.Context, dir string, files map[string][]byte) error {
	tok, err := g.token(ctx)
	if err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(files))
	if i := slices.Index(names, "meta.json"); i >= 0 {
		names = append(append(names[:i:i], names[i+1:]...), "meta.json")
	}
	for _, name := range names {
		if err := g.write(ctx, tok, path.Join(g.Prefix, dir, name), contentType(name), files[name]); err != nil {
			return err
		}
	}
	return nil
}

// retryWait is the pause before each retry; tests shorten it.
var retryWait = []time.Duration{time.Second, 3 * time.Second}

func (g *GCS) write(ctx context.Context, tok, obj, ctype string, data []byte) error {
	base := g.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	u := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&ifGenerationMatch=0&name=%s",
		strings.TrimSuffix(base, "/"), url.PathEscape(g.Bucket), url.QueryEscape(obj))
	var last error
	for attempt := 0; attempt <= len(retryWait); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("writing gs://%s/%s: %w (after %v)", g.Bucket, obj, ctx.Err(), last)
			case <-time.After(retryWait[attempt-1]):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", ctype)
		resp, err := g.client().Do(req)
		if err != nil {
			last = fmt.Errorf("writing gs://%s/%s: %w", g.Bucket, obj, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusPreconditionFailed && attempt > 0:
			return nil // the attempt before wrote it
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = fmt.Errorf("writing gs://%s/%s: %s: %s", g.Bucket, obj, resp.Status, strings.TrimSpace(string(body)))
			continue
		}
		return fmt.Errorf("writing gs://%s/%s: %s: %s", g.Bucket, obj, resp.Status, strings.TrimSpace(string(body)))
	}
	return last
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
