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

package scenario

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stream is an event stream shaped like mast's: the incident from
// lookout, the coordinator delegating, a specialist's tool call and its
// result (role "user", from the specialist), a function response from
// "user" with no text, the coordinator's answer, then a repeat of the
// incident and a one-line acknowledgement.
const stream = `event: capabilities
data: {"protocol_version":"1.7.0"}

event: usage-update
data: {"tokens_in_total":10,"cost_usd_total":0.042,"turns_total":3}

event: agent
data: {"seq":3,"event":{"author":"user","content":{"role":"user","parts":[{"text":"{\"kind\":\"k8s-event\",\"name\":\"missing-env-1021-120000-abc\"}"}]}}}

data: {"seq":4,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"functionCall":{"name":"_fallback"}}]}}}

data: {"seq":7,"event":{"author":"_fallback","content":{"role":"model","parts":[{"text":"looking at the pod"}]}}}

data: {"seq":9,"event":{"author":"_fallback","content":{"role":"user","parts":[{"text":"tool result text"}]}}}

data: {"seq":23,"event":{"author":"user","content":{"role":"user","parts":[{"functionResponse":{}}]}}}

data: {"seq":29,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"text":"Cause: DATABASE_URL is missing from ConfigMap missing-env-1021-120000-config."}]}}}

data: {"seq":30,"event":{"author":"user","content":{"role":"user","parts":[{"text":"{\"kind\":\"k8s-event\",\"repeat\":true}"}]}}}

data: {"seq":31,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"text":"Still failing: same incident."}]}}}

data: not json
`

func TestParseEventsAndAnswer(t *testing.T) {
	tr, err := ParseEvents(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Events) != 8 || tr.Cost != 0.042 {
		t.Fatalf("%d events, cost %v", len(tr.Events), tr.Cost)
	}
	answer, superseded := tr.Answer()
	if answer != "Cause: DATABASE_URL is missing from ConfigMap missing-env-1021-120000-config." || !superseded {
		t.Errorf("Answer = %q, superseded=%v", answer, superseded)
	}
	if !tr.Mentions("missing-env-1021-120000-abc") || tr.Mentions("other-workload") {
		t.Error("Mentions wrong")
	}
}

func TestAnswerEdgeCases(t *testing.T) {
	parse := func(s string) *Transcript {
		t.Helper()
		tr, err := ParseEvents(strings.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	// No incoming message yet.
	if a, _ := parse(`data: {"seq":1,"event":{"author":"x","content":{"role":"model","parts":[{"text":"hi"}]}}}`).Answer(); a != "" {
		t.Errorf("answer without a message = %q", a)
	}
	// Still working: no answer, not superseded.
	if a, s := parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}`).Answer(); a != "" || s {
		t.Errorf("answer before any reply = %q, %v", a, s)
	}
	// No coordinator: the last non-user text counts.
	tr := parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}
data: {"seq":2,"event":{"author":"agent","content":{"role":"model","parts":[{"text":"first"},{"text":"second"}]}}}`)
	if a, s := tr.Answer(); a != "first\nsecond" || s {
		t.Errorf("answer = %q, %v", a, s)
	}
	// Another message before any answer joins the turn (lookout's update
	// while the agent investigates); the specialist's text doesn't count
	// when a coordinator answers later.
	tr = parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}
data: {"seq":2,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"functionCall":{"name":"_fallback"}}]}}}
data: {"seq":3,"event":{"author":"_fallback","content":{"role":"model","parts":[{"text":"looking"}]}}}
data: {"seq":4,"event":{"author":"user","content":{"role":"user","parts":[{"text":"update"}]}}}
data: {"seq":5,"event":{"author":"gke-triage_coordinator","content":{"role":"model","parts":[{"text":"diagnosis"}]}}}`)
	if a, s := tr.Answer(); a != "diagnosis" || s {
		t.Errorf("answer with a mid-turn message = %q, %v", a, s)
	}
	// Without a coordinator, any text is an answer, so the next message
	// supersedes it.
	tr = parse(`data: {"seq":1,"event":{"author":"user","content":{"role":"user","parts":[{"text":"incident"}]}}}
data: {"seq":2,"event":{"author":"agent","content":{"role":"model","parts":[{"text":"diagnosis"}]}}}
data: {"seq":3,"event":{"author":"user","content":{"role":"user","parts":[{"text":"repeat"}]}}}
data: {"seq":4,"event":{"author":"agent","content":{"role":"model","parts":[{"text":"ack"}]}}}`)
	if a, s := tr.Answer(); a != "diagnosis" || !s {
		t.Errorf("answer without a coordinator = %q, %v", a, s)
	}
}

func TestRouter(t *testing.T) {
	var gotActor, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor, gotAuth = r.Header.Get("ate-target-actor"), r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/sessions":
			fmt.Fprint(w, `{"sessions":[{"sessionID":"s1","status":"active","app":"mast"}]}`)
		case "/sessions/s1/status":
			fmt.Fprint(w, `{"state":"idle"}`)
		case "/sessions/s1/events":
			fmt.Fprint(w, stream)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stays open, as mast's does
		default:
			http.Error(w, "no such session", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r := &Router{Base: srv.URL + "/", Atespace: "agents", Token: "tok", EventsWait: 200 * time.Millisecond}
	ctx := context.Background()

	sessions, err := r.Sessions(ctx, "lookout-1")
	if err != nil || len(sessions) != 1 || sessions[0].ID != "s1" || sessions[0].Status != "active" {
		t.Fatalf("Sessions = %+v, %v", sessions, err)
	}
	if gotActor != "agents/lookout-1" || gotAuth != "Bearer tok" {
		t.Errorf("headers %q, %q", gotActor, gotAuth)
	}
	if state, err := r.State(ctx, "lookout-1", "s1"); err != nil || state != "idle" {
		t.Errorf("State = %q, %v", state, err)
	}
	tr, err := r.Events(ctx, "lookout-1", "s1")
	if err != nil || len(tr.Events) != 8 {
		t.Fatalf("Events = %v, %v", tr, err)
	}
	if _, err := r.State(ctx, "lookout-1", "nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing session: err = %v", err)
	}
	if _, err := r.Events(ctx, "lookout-1", "nope"); err == nil {
		t.Error("Events on a missing session succeeded")
	}
	if _, err := (&Router{Base: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}}).Sessions(ctx, "x"); err == nil {
		t.Error("unreachable router succeeded")
	}
	if _, err := (&Router{Base: "::bad"}).Sessions(ctx, "x"); err == nil {
		t.Error("bad URL accepted")
	}
}
