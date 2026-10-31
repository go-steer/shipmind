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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Router talks to an agent's session API (mast's attach surface)
// through Substrate's router, which wakes a suspended task on the way.
type Router struct {
	// Base is the router's URL ("http://atenet-router.ate-system.svc.cluster.local").
	Base string
	// Atespace is the agents' ax atespace.
	Atespace string
	// Token is the agent's attach token.
	Token string
	// Client defaults to one with a timeout long enough for a cold wake.
	Client *http.Client
	// EventsWait bounds how long Events reads the event stream, which
	// replays the session and then stays open for new events.
	EventsWait time.Duration
}

// Session is one row of GET /sessions.
type Session struct {
	ID string `json:"sessionID"`
	// Status is "active" for a session in mast's memory and "idle" for
	// one known only from its store (after a restart, before it is used).
	Status string `json:"status"`
}

// routerTimeout is long enough for the router to wake a suspended task.
const routerTimeout = 120 * time.Second

func (r *Router) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: routerTimeout}
}

func (r *Router) get(ctx context.Context, task, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(r.Base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("ate-target-actor", r.Atespace+"/"+task)
	req.Header.Set("Authorization", "Bearer "+r.Token)
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s on %s: %s: %s", path, task, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (r *Router) getJSON(ctx context.Context, task, path string, v any) error {
	resp, err := r.get(ctx, task, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Sessions lists a task's sessions.
func (r *Router) Sessions(ctx context.Context, task string) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	if err := r.getJSON(ctx, task, "/sessions", &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// State returns a session's agent state ("idle" when not working).
func (r *Router) State(ctx context.Context, task, session string) (string, error) {
	var out struct {
		State string `json:"state"`
	}
	if err := r.getJSON(ctx, task, "/sessions/"+url.PathEscape(session)+"/status", &out); err != nil {
		return "", err
	}
	return out.State, nil
}

// Events reads a session's event stream for EventsWait (default 15s).
func (r *Router) Events(ctx context.Context, task, session string) (*Transcript, error) {
	wait := r.EventsWait
	if wait == 0 {
		wait = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := r.get(ctx, task, "/sessions/"+url.PathEscape(session)+"/events")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	t, err := ParseEvents(resp.Body)
	// The stream doesn't end on its own; running out of time is how a
	// read finishes.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, err
	}
	return t, nil
}

// Transcript is a session's events, as far as scoring needs them.
type Transcript struct {
	Events []Event
	// Cost is the session's running cost in US dollars, from the last
	// usage update.
	Cost float64
}

// Event is one agent event: who wrote it and its text parts.
type Event struct {
	Seq    int
	Author string
	Role   string
	Texts  []string
}

// ParseEvents reads mast's server-sent events: "data:" lines carrying an
// object with a seq are session events; one carrying cost_usd_total is a
// usage update. Other lines are ignored. It returns what it parsed when
// the stream fails, along with the error.
func ParseEvents(r io.Reader) (*Transcript, error) {
	t := &Transcript{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var d struct {
			Seq   *int `json:"seq"`
			Event struct {
				Author  string `json:"author"`
				Content *struct {
					Role  string `json:"role"`
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"event"`
			Cost *float64 `json:"cost_usd_total"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			continue
		}
		if d.Cost != nil {
			t.Cost = *d.Cost
		}
		if d.Seq == nil {
			continue
		}
		e := Event{Seq: *d.Seq, Author: d.Event.Author}
		if c := d.Event.Content; c != nil {
			e.Role = c.Role
			for _, p := range c.Parts {
				if p.Text != "" {
					e.Texts = append(e.Texts, p.Text)
				}
			}
		}
		t.Events = append(t.Events, e)
	}
	return t, sc.Err()
}

// isUser reports whether e is a message sent to the agent: authored by
// "user" and carrying text. Tool results carry role "user" too, from the
// specialist that called the tool, and function responses come from
// "user" with no text; neither is a new message.
func isUser(e Event) bool { return e.Author == "user" && len(e.Texts) > 0 }

// Answer returns the agent's answer to the first message a person or
// lookout sent: the coordinator's last text before the next such message
// that follows it (lookout re-sends an incident while it stays broken,
// and the agent answers a repeat with a one-line acknowledgement, which
// must not count as the diagnosis). A message arriving before any answer
// joins the same turn: lookout sends updates about the incident while
// the agent is still investigating, and the answer then covers both.
// Without an author ending in "_coordinator", the last non-user text
// counts. It also returns whether a later message arrived after the
// answer, meaning the answer is complete.
func (t *Transcript) Answer() (answer string, superseded bool) {
	first := -1
	hasCoordinator := false
	for i, e := range t.Events {
		if first < 0 && isUser(e) {
			first = i
		}
		if strings.HasSuffix(e.Author, "_coordinator") {
			hasCoordinator = true
		}
	}
	if first < 0 {
		return "", false
	}
	var coordinator, other string
	for _, e := range t.Events[first+1:] {
		if isUser(e) {
			if coordinator != "" || (!hasCoordinator && other != "") {
				superseded = true
				break
			}
			continue
		}
		if len(e.Texts) == 0 || e.Author == "user" || e.Role == "user" {
			continue
		}
		text := strings.Join(e.Texts, "\n")
		if strings.HasSuffix(e.Author, "_coordinator") {
			coordinator = text
		}
		other = text
	}
	if coordinator != "" {
		return coordinator, superseded
	}
	return other, superseded
}

// Mentions reports whether any event's text contains s, for finding the
// session an incident landed in.
func (t *Transcript) Mentions(s string) bool {
	for _, e := range t.Events {
		for _, text := range e.Texts {
			if strings.Contains(text, s) {
				return true
			}
		}
	}
	return false
}

// runNameSuffix matches what RunName appends: -MMDD-HHMMSS.
var runNameSuffix = regexp.MustCompile(`^-?\d{4}-\d{6}`)

// mentionsRun reports whether text names a run workload of a scenario,
// given prefix "<scenario>-": the prefix followed by a run timestamp.
func mentionsRun(text, prefix string) bool {
	for rest := text; ; {
		i := strings.Index(rest, prefix)
		if i < 0 {
			return false
		}
		rest = rest[i+len(prefix):]
		if runNameSuffix.MatchString(rest) {
			return true
		}
	}
}

// FirstMessage returns the first message sent to the agent: for a
// lookout incident task, the incident itself.
func (t *Transcript) FirstMessage() string {
	for _, e := range t.Events {
		if isUser(e) {
			return strings.Join(e.Texts, "\n")
		}
	}
	return ""
}

// Incident is the identity of the incident lookout opened a task for,
// read from the task's first message (lookout's inject payload).
type Incident struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// FirstIncident parses the task's first message as a lookout incident.
// ok is false when it isn't one; a storm (kind "storm") never is: a
// storm task is keyed on a shared ancestor (a Node, a namespace, the
// cluster), lives across storms and lists many members, so it is never
// a scenario run's own task.
func (t *Transcript) FirstIncident() (Incident, bool) {
	var in Incident
	if err := json.Unmarshal([]byte(t.FirstMessage()), &in); err != nil || in.Name == "" || in.Kind == "" || in.Kind == "storm" {
		return Incident{}, false
	}
	return in, true
}

// About reports whether the incident is about the scenario workload
// workload in namespace ns: the object it names is the workload or one
// of its pods ("<workload>-<replicaset hash>-<pod hash>").
func (in Incident) About(ns, workload string) bool {
	return in.Namespace == ns && (in.Name == workload || strings.HasPrefix(in.Name, workload+"-"))
}
