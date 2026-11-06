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

package archive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct{ in, bucket, prefix string }{
		{"gs://runs/stage", "runs", "stage"},
		{"gs://runs/stage/", "runs", "stage"},
		{"gs://runs/a/b", "runs", "a/b"},
		{"gs://runs", "runs", ""},
	} {
		g, err := Parse(tc.in)
		if err != nil || g.Bucket != tc.bucket || g.Prefix != tc.prefix {
			t.Errorf("Parse(%q) = %+v, %v", tc.in, g, err)
		}
	}
	for _, bad := range []string{"", "runs/stage", "s3://runs/x", "gs://", "gs:///x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestSave(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{} // object -> content type + body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Header.Get("Metadata-Flavor") != "Google" {
				http.Error(w, "no flavor", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3599}`)
		case "/upload/storage/v1/b/runs/o":
			q := r.URL.Query()
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer tok" || q.Get("uploadType") != "media" || q.Get("ifGenerationMatch") != "0" {
				http.Error(w, "bad request "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			if q.Get("name") == "stage/s/2026-11-06/w/exists.json" {
				http.Error(w, "conditionNotMet", http.StatusPreconditionFailed)
				return
			}
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got[q.Get("name")] = r.Header.Get("Content-Type") + " " + string(b)
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &GCS{Bucket: "runs", Prefix: "stage", TokenURL: srv.URL + "/token", APIBase: srv.URL}
	err := g.Save(context.Background(), "s/2026-11-06/w", map[string][]byte{
		"meta.json": []byte(`{"a":1}`), "transcript.sse": []byte("data: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["stage/s/2026-11-06/w/meta.json"] != `application/json {"a":1}` || got["stage/s/2026-11-06/w/transcript.sse"] != "text/event-stream data: {}\n" {
		t.Errorf("objects %q", got)
	}
	// An object already there is never overwritten, and the error says which.
	err = g.Save(context.Background(), "s/2026-11-06/w", map[string][]byte{"exists.json": []byte("{}")})
	if err == nil || !strings.Contains(err.Error(), "gs://runs/stage/s/2026-11-06/w/exists.json") || !strings.Contains(err.Error(), "412") {
		t.Errorf("existing object: %v", err)
	}
	// Without a token, nothing is written.
	bad := &GCS{Bucket: "runs", TokenURL: srv.URL + "/nope", APIBase: srv.URL}
	if err := bad.Save(context.Background(), "x", map[string][]byte{"a.json": nil}); err == nil || !strings.Contains(err.Error(), "archive token") {
		t.Errorf("no token: %v", err)
	}
}

// Objects go in name order with meta.json last, so a run with meta.json
// is complete; a transient failure is retried, and a 412 on the retry
// means the attempt before landed.
func TestSaveOrderAndRetries(t *testing.T) {
	old := retryWait
	retryWait = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryWait = old })
	var mu sync.Mutex
	var order []string
	tries := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"access_token":"tok"}`)
			return
		}
		name := r.URL.Query().Get("name")
		mu.Lock()
		tries[name]++
		n := tries[name]
		mu.Unlock()
		switch {
		case strings.HasSuffix(name, "flaky.json") && n == 1:
			http.Error(w, "backend", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(name, "landed.json") && n == 1:
			// Written, but the answer was lost.
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			http.Error(w, "reset", http.StatusBadGateway)
			return
		case strings.HasSuffix(name, "landed.json"):
			http.Error(w, "conditionNotMet", http.StatusPreconditionFailed)
			return
		case strings.HasSuffix(name, "down.json"):
			http.Error(w, "backend", http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	g := &GCS{Bucket: "runs", TokenURL: srv.URL + "/token", APIBase: srv.URL}
	err := g.Save(context.Background(), "d", map[string][]byte{"meta.json": nil, "a.json": nil, "flaky.json": nil, "landed.json": nil, "z.sse": nil})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"d/a.json", "d/flaky.json", "d/landed.json", "d/z.sse", "d/meta.json"}; !slices.Equal(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
	if err := g.Save(context.Background(), "e", map[string][]byte{"down.json": nil, "meta.json": nil}); err == nil || !strings.Contains(err.Error(), "503") || tries["e/meta.json"] != 0 {
		t.Errorf("a write that keeps failing: %v; meta.json tried %d times", err, tries["e/meta.json"])
	}
}
