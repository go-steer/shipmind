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
	"strings"
	"sync"
	"testing"
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
