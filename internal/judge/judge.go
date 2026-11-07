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

// Package judge grades a scenario's answer against its ground-truth
// diagnosis with a model (#90): a 1-5 score, whether the diagnosis is
// correct, specific and actionable, and the reasoning. The rubric is
// core-agent's response-quality rubric, copied rather than imported, so
// that a scenario's scores stay comparable over time: a judge whose
// wording moved with an upstream release would move the trend with it
// (#91). Change Rubric only with a new RubricID.
//
// The judge model is Claude on Vertex AI, a different model family from
// the agent's, so no model grades its own work.
package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/vertex"
	"golang.org/x/oauth2/google"
)

// RubricID names the rubric below: where it came from and its revision.
const RubricID = "core-agent-quality@ae922c2b"

// Rubric is core-agent's pkg/evals/judge QualityPrompt at ae922c2b
// (go-steer/core-agent#1538), verbatim: the expected response, then the
// answer, as its two %s.
const Rubric = `You are evaluating an SRE agent's response to a Kubernetes incident or health-check query.

## Expected response (ground truth)
%s

## Agent response (to evaluate)
%s

## Scoring rubric (1–5)
5 — Matches expected diagnosis; names specific resources; gives actionable remediation steps
4 — Correct diagnosis; mostly specific; remediation present but vague in one area
3 — Partially correct; some specifics missing; some actionable advice
2 — Diagnosis is off or too generic; little actionable content
1 — Wrong diagnosis or irrelevant response

Evaluate and return structured output.

Reply with a single JSON object and nothing else:
{"reasoning": "<step-by-step reasoning for the score>", "score": <integer 1-5>, "specific": <true if the response names specific resources such as pod names or namespaces>, "actionable": <true if the response gives clear next steps or remediation advice>, "correct_diagnosis": <true if the root cause identified matches the expected response>}`

// schema is the reply the rubric asks for, enforced with structured
// outputs rather than parsed out of free text.
var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"reasoning":         map[string]any{"type": "string"},
		"score":             map[string]any{"type": "integer", "enum": []int{1, 2, 3, 4, 5}},
		"specific":          map[string]any{"type": "boolean"},
		"actionable":        map[string]any{"type": "boolean"},
		"correct_diagnosis": map[string]any{"type": "boolean"},
	},
	"required":             []string{"reasoning", "score", "specific", "actionable", "correct_diagnosis"},
	"additionalProperties": false,
}

// Grade is one answer's grade, as a run records it.
type Grade struct {
	// Model and Rubric say what graded it, so a re-grade (#91) can tell
	// grades it may compare from ones it must not.
	Model  string `json:"model"`
	Rubric string `json:"rubric"`
	Effort string `json:"effort,omitempty"`

	// Score is 1-5; 0 with Error set means no grade, and then the three
	// flags below are not findings either.
	Score            int    `json:"score,omitempty"`
	CorrectDiagnosis bool   `json:"correct_diagnosis"`
	Specific         bool   `json:"specific"`
	Actionable       bool   `json:"actionable"`
	Reasoning        string `json:"reasoning,omitempty"`

	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// CostUSD is at Anthropic's list prices for the model; Vertex AI
	// bills Claude at its own rates, usually the same. Zero for a model
	// without a known price.
	CostUSD float64 `json:"cost_usd"`

	// Error says why there is no score: the call failed or the reply
	// didn't fit the rubric.
	Error string `json:"error,omitempty"`
}

// Messages is the part of the Anthropic client a Judge calls.
type Messages interface {
	New(ctx context.Context, body anthropic.MessageNewParams, opts ...option.RequestOption) (*anthropic.Message, error)
}

// Judge grades answers with one model at one effort.
type Judge struct {
	Model    string
	Effort   string
	Messages Messages
}

// DefaultEffort is the judge's effort when none is given: grading is one
// call a run, so depth costs little.
const DefaultEffort = "high"

// NewVertex returns a Judge calling model on Vertex AI in project and
// region ("global" unless set), with Application Default Credentials: a
// workstation's gcloud login, or a pod's workload identity.
func NewVertex(ctx context.Context, model, project, region, effort string) (j *Judge, err error) {
	if model == "" || project == "" {
		return nil, errors.New("judge: want a model and a Google Cloud project")
	}
	if region == "" {
		region = "global"
	}
	if effort == "" {
		effort = DefaultEffort
	}
	if err := CheckEffort(effort); err != nil {
		return nil, err
	}
	// vertex.WithGoogleAuth panics without credentials; finding them here
	// makes that an error.
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("judge: Google credentials: %w", err)
	}
	// It still panics if it can't build the HTTP client (a bad client
	// certificate setting): an error too, so a judge never stops a run.
	defer func() {
		if p := recover(); p != nil {
			j, err = nil, fmt.Errorf("judge: Vertex AI client: %v", p)
		}
	}()
	c := anthropic.NewClient(vertex.WithCredentials(ctx, region, project, creds))
	return &Judge{Model: model, Effort: effort, Messages: &c.Messages}, nil
}

// Broken is a Grader that couldn't be set up: every grade it returns
// names the model and says why there is no score, so a run records a
// misconfigured judge instead of failing over it.
type Broken struct {
	Model string
	Err   error
}

// Grade implements the scenario runner's Grader.
func (b Broken) Grade(context.Context, string, string) (Grade, error) {
	return Grade{Model: b.Model, Rubric: RubricID, Error: b.Err.Error()}, b.Err
}

// CheckEffort checks an effort level the API accepts.
func CheckEffort(e string) error {
	switch anthropic.OutputConfigEffort(e) {
	case anthropic.OutputConfigEffortLow, anthropic.OutputConfigEffortMedium, anthropic.OutputConfigEffortHigh,
		anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortMax:
		return nil
	}
	return fmt.Errorf("judge: effort %q: want low, medium, high, xhigh or max", e)
}

// Grade grades answer against expected, the scenario's ground truth. It
// always returns a Grade naming the model and rubric; on an error the
// Grade has no score and the error is in it too.
func (j *Judge) Grade(ctx context.Context, expected, answer string) (Grade, error) {
	g := Grade{Model: j.Model, Rubric: RubricID, Effort: j.Effort}
	fail := func(err error) (Grade, error) {
		g.Error = err.Error()
		return g, err
	}
	if strings.TrimSpace(expected) == "" {
		return fail(errors.New("judge: no expected diagnosis to grade against"))
	}
	if strings.TrimSpace(answer) == "" {
		g.Score, g.Reasoning = 1, "the run produced no final text, so there is nothing to grade"
		return g, nil
	}
	params := anthropic.MessageNewParams{
		Model:     j.Model,
		MaxTokens: 16000,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(fmt.Sprintf(Rubric, expected, answer))),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	}
	if j.Effort != "" {
		params.OutputConfig.Effort = anthropic.OutputConfigEffort(j.Effort)
	}
	msg, err := j.Messages.New(ctx, params)
	if err != nil {
		return fail(fmt.Errorf("judge: %s: %w", j.Model, err))
	}
	g.InputTokens = msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens
	g.OutputTokens = msg.Usage.OutputTokens
	g.CostUSD = cost(j.Model, msg.Usage)
	if msg.StopReason == anthropic.StopReasonRefusal {
		return fail(fmt.Errorf("judge: %s declined to grade", j.Model))
	}
	if msg.StopReason == anthropic.StopReasonMaxTokens {
		return fail(fmt.Errorf("judge: %s ran out of output tokens", j.Model))
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	var reply struct {
		Reasoning        string `json:"reasoning"`
		Score            int    `json:"score"`
		Specific         bool   `json:"specific"`
		Actionable       bool   `json:"actionable"`
		CorrectDiagnosis bool   `json:"correct_diagnosis"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text.String())), &reply); err != nil {
		return fail(fmt.Errorf("judge: %s replied with no grade (%w): %q", j.Model, err, truncate(text.String(), 200)))
	}
	if reply.Score < 1 || reply.Score > 5 {
		return fail(fmt.Errorf("judge: %s scored %d, outside the rubric's 1-5", j.Model, reply.Score))
	}
	g.Score, g.Reasoning = reply.Score, reply.Reasoning
	g.Specific, g.Actionable, g.CorrectDiagnosis = reply.Specific, reply.Actionable, reply.CorrectDiagnosis
	return g, nil
}

// prices are list prices per million tokens: input, output, cache read.
var prices = map[string][3]float64{
	"claude-opus-5-5":   {4, 20, 0.20},
	"claude-sonnet-5-5": {2, 10, 0.20},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01},
}

func cost(model string, u anthropic.Usage) float64 {
	p, ok := prices[model]
	if !ok {
		return 0
	}
	// Cache writes cost more than plain input, but the judge sends no
	// cache_control, so there are none to price.
	in := float64(u.InputTokens + u.CacheCreationInputTokens)
	return (in*p[0] + float64(u.OutputTokens)*p[1] + float64(u.CacheReadInputTokens)*p[2]) / 1e6
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
