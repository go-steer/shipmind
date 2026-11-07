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

package judge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// fakeMessages replies with a message built from reply (or err), and
// keeps the request it got.
type fakeMessages struct {
	reply string
	err   error
	got   *anthropic.MessageNewParams
	calls int
}

func (f *fakeMessages) New(_ context.Context, body anthropic.MessageNewParams, _ ...option.RequestOption) (*anthropic.Message, error) {
	f.calls++
	f.got = &body
	if f.err != nil {
		return nil, f.err
	}
	var m anthropic.Message
	if err := json.Unmarshal([]byte(f.reply), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// message is a Messages API reply with one text block.
func message(stop, text string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5",
		"stop_reason": stop,
		"content":     []map[string]any{{"type": "text", "text": text}},
		"usage":       map[string]any{"input_tokens": 1000, "output_tokens": 300, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
	})
	return string(b)
}

const good = `{"reasoning": "names the ConfigMap and the fix", "score": 5, "specific": true, "actionable": true, "correct_diagnosis": true}`

func TestGrade(t *testing.T) {
	f := &fakeMessages{reply: message("end_turn", good)}
	j := &Judge{Model: "claude-sonnet-5-5", Effort: "high", Messages: f}
	g, err := j.Grade(context.Background(), "DATABASE_URL is missing from ConfigMap x-config", "the answer under test")
	if err != nil {
		t.Fatal(err)
	}
	if g.Score != 5 || !g.CorrectDiagnosis || !g.Specific || !g.Actionable || g.Reasoning == "" || g.Error != "" {
		t.Errorf("grade %+v", g)
	}
	if g.Model != "claude-sonnet-5-5" || g.Rubric != RubricID || g.Effort != "high" {
		t.Errorf("grade doesn't say what graded it: %+v", g)
	}
	// 1000 in at $2/M and 300 out at $10/M.
	if g.InputTokens != 1000 || g.OutputTokens != 300 || math.Abs(g.CostUSD-0.005) > 1e-9 {
		t.Errorf("usage %d/%d, cost %v", g.InputTokens, g.OutputTokens, g.CostUSD)
	}

	// The request: the rubric around both texts, structured output, the
	// effort, and no sampling parameters (Claude 5.5 models reject them).
	req, _ := json.Marshal(f.got)
	var body map[string]any
	if err := json.Unmarshal(req, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "claude-sonnet-5-5" {
		t.Errorf("model %v", body["model"])
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "thinking"} {
		if _, ok := body[k]; ok {
			t.Errorf("request sets %s", k)
		}
	}
	oc, _ := body["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if oc["effort"] != "high" || format["type"] != "json_schema" || format["schema"] == nil {
		t.Errorf("output_config %v", oc)
	}
	prompt := string(req)
	if !strings.Contains(prompt, "DATABASE_URL is missing from ConfigMap x-config") || !strings.Contains(prompt, "the answer under test") ||
		!strings.Contains(prompt, "Scoring rubric (1–5)") {
		t.Errorf("prompt %s", prompt)
	}
}

func TestGradeFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fakeMessages
		want string
	}{
		{"api error", &fakeMessages{err: errors.New("403 permission denied")}, "403 permission denied"},
		{"refusal", &fakeMessages{reply: message("refusal", "")}, "declined"},
		{"cut off", &fakeMessages{reply: message("max_tokens", `{"reasoning": "`)}, "ran out of output tokens"},
		{"not json", &fakeMessages{reply: message("end_turn", "Score: 5")}, "no grade"},
		{"off the rubric", &fakeMessages{reply: message("end_turn", `{"reasoning": "x", "score": 7}`)}, "outside the rubric"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &Judge{Model: "claude-opus-5-5", Messages: tc.f}
			g, err := j.Grade(context.Background(), "expected", "answer")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if g.Score != 0 || g.Error != err.Error() || g.Model != "claude-opus-5-5" || g.Rubric != RubricID {
				t.Errorf("grade %+v", g)
			}
		})
	}
}

// An empty answer scores 1 without a call; no ground truth is an error.
func TestGradeWithoutCall(t *testing.T) {
	f := &fakeMessages{}
	j := &Judge{Model: "m", Messages: f}
	g, err := j.Grade(context.Background(), "expected", "  \n")
	if err != nil || g.Score != 1 || f.calls != 0 {
		t.Errorf("empty answer: %+v, %v, %d calls", g, err, f.calls)
	}
	if _, err := j.Grade(context.Background(), "", "answer"); err == nil || f.calls != 0 {
		t.Errorf("no ground truth: %v, %d calls", err, f.calls)
	}
}

func TestNewVertexChecks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ model, project, effort, want string }{
		{"", "p", "", "want a model"},
		{"m", "", "", "want a model"},
		{"m", "p", "extreme", "effort"},
	} {
		if _, err := NewVertex(ctx, tc.model, tc.project, "", tc.effort); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("NewVertex(%q, %q, effort %q): %v, want %q", tc.model, tc.project, tc.effort, err, tc.want)
		}
	}
}

func TestBroken(t *testing.T) {
	g, err := Broken{Model: "m", Err: errors.New("no credentials")}.Grade(context.Background(), "e", "a")
	if err == nil || g.Model != "m" || g.Error != "no credentials" || g.Score != 0 {
		t.Errorf("%+v, %v", g, err)
	}
}

func TestCost(t *testing.T) {
	u := anthropic.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadInputTokens: 1_000_000}
	if c := cost("claude-opus-5-5", u); math.Abs(c-(4+2+0.2)) > 1e-9 {
		t.Errorf("opus cost %v", c)
	}
	if c := cost("some-other-model", u); c != 0 {
		t.Errorf("unknown model cost %v", c)
	}
}

// The rubric keeps core-agent's two slots, in order: ground truth, then
// the answer.
func TestRubricSlots(t *testing.T) {
	if strings.Count(Rubric, "%s") != 2 || strings.Index(Rubric, "Expected response") > strings.Index(Rubric, "Agent response") {
		t.Error("Rubric no longer has core-agent's two slots in order")
	}
}

func TestTruncateKeepsRunes(t *testing.T) {
	if got := truncate("ééé", 2); got != "éé…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("ok", 5); got != "ok" {
		t.Errorf("truncate = %q", got)
	}
}
