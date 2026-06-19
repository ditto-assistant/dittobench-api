// LLM-judge scoring — a faithful port of the dittobench-starter-kit judge
// (src/judge.rs), which itself ports the Ditto backend DittoBench judges
// (backend/pkg/dittobench/scorers/judge.go + longmemeval.go) that produced the
// published DittoBench numbers.
//
// Two judges, both structured and called at temperature 0:
//   - JudgeMemory — LongMemEval QA: does the response contain the correct
//     answer? Returns yes/no. Uses the verbatim system prompt + per-question-type
//     clauses.
//   - JudgeToolQuality — scores the assistant's final text after tool use on
//     helpfulness + accuracy (1–5 each); the mean/5 is the judge half of the
//     tool composite (tool case = 0.5*tool_accuracy + 0.5*quality).
//
// The judge model defaults to SCORER_MODEL (google/gemini-3.1-flash-lite).
package scorer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// LLM is the minimal chat interface the judge needs. *llm.Client satisfies it.
type LLM interface {
	Complete(ctx context.Context, model, system, user string) (string, error)
}

const toolJudgeSystem = "You are a rigorous evaluator scoring an AI assistant's response after it used various tools.\nReturn ONLY valid JSON matching this schema:\n{\"helpfulness\":<1-5>,\"accuracy\":<1-5>,\"rationale\":\"<short>\"}\n\nScoring guide (integer 1-5):\n- helpfulness: Is the response helpful and complete given the user's request?\n  5 = fully answers the question with useful detail; 3 = adequate but unremarkable; 1 = unhelpful\n- accuracy: Does the response accurately use information from the tools it called?\n  5 = all claims well-supported by tool results; 3 = mostly accurate; 1 = fabricated or contradicts tools\n\nBe critical. Most competent responses score 3-4. Score 5 only when genuinely impressive."

const lmeJudgeBase = "You are a rigorous evaluator for a long-term memory benchmark.\nYou will be given a question, the correct answer, and a model's response.\nDetermine if the model's response contains the correct answer.\n\nReturn ONLY valid JSON: {\"correct\":\"yes\"|\"no\",\"rationale\":\"<brief reason>\"}"

// buildLMEJudgeSystem appends the question-type clause (same first-match order
// as the backend / judge.rs lme_judge_system).
func buildLMEJudgeSystem(questionType string, isAbstention bool) string {
	qt := strings.ToLower(questionType)
	var clause string
	switch {
	case strings.Contains(qt, "temporal"):
		clause = "\n\nIMPORTANT: For temporal reasoning questions, do NOT penalize off-by-one errors in day counts. If the correct answer says \"5 days\" and the response says \"4 days\" or \"6 days\", that is still correct. Focus on whether the temporal reasoning approach and the approximate answer are right."
	case strings.Contains(qt, "knowledge-update"), strings.Contains(qt, "knowledge_update"):
		clause = "\n\nIMPORTANT: For knowledge update questions, the response may contain both outdated and updated information. As long as the UPDATED (most recent) information matches the correct answer, mark as correct. The presence of older information alongside the correct update is acceptable."
	case strings.Contains(qt, "preference"):
		clause = "\n\nIMPORTANT: For preference questions, check whether the user's preference information was correctly recalled and utilized in the response. The response need not quote the preference verbatim — it should demonstrate that the preference was understood and applied appropriately."
	case isAbstention:
		clause = "\n\nIMPORTANT: This is an ABSTENTION question. The correct behavior is for the model to indicate that it does not have enough information to answer, or that the question involves unknown information. Mark as correct if the response appropriately indicates uncertainty, declines to answer, or states the information is not available. Mark as incorrect if the model fabricates an answer."
	}
	return lmeJudgeBase + clause
}

// JudgeMemory runs the LongMemEval QA judge. An empty response → false. A judge
// error → false (matches the backend, where a failed judge contributes 0).
func JudgeMemory(ctx context.Context, model LLM, modelID, question, correctAnswer, response, questionType string) bool {
	if strings.TrimSpace(response) == "" {
		return false
	}
	isAbstention := strings.Contains(strings.ToLower(questionType), "abstention")
	system := buildLMEJudgeSystem(questionType, isAbstention)
	user := fmt.Sprintf("QUESTION:\n%s\n\nCORRECT ANSWER:\n%s\n\nMODEL RESPONSE:\n%s\n\nReturn the JSON verdict.", question, correctAnswer, response)

	text, err := model.Complete(ctx, modelID, system, user)
	if err != nil {
		return false
	}
	v := extractJSON(text)
	if v == nil {
		return false
	}
	c, ok := v["correct"].(string)
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(c), "yes")
}

// JudgeToolQuality runs the tool-use response-quality judge. Returns a 0–1 mean
// of the non-zero helpfulness + accuracy dims. Judge error / no text → 0.0.
func JudgeToolQuality(ctx context.Context, model LLM, modelID, prompt string, toolsCalled []string, expectedBehavior, response string) float64 {
	if strings.TrimSpace(response) == "" {
		return 0.0
	}
	tools := "none"
	if len(toolsCalled) > 0 {
		tools = strings.Join(toolsCalled, ", ")
	}
	expected := "No specific expected behavior defined."
	if strings.TrimSpace(expectedBehavior) != "" {
		expected = expectedBehavior
	}
	user := fmt.Sprintf("USER PROMPT:\n%s\n\nTOOLS CALLED: %s\n\nEXPECTED BEHAVIOR:\n%s\n\nASSISTANT RESPONSE:\n%s\n\nReturn the JSON score object.", prompt, tools, expected, response)

	text, err := model.Complete(ctx, modelID, toolJudgeSystem, user)
	if err != nil {
		return 0.0
	}
	v := extractJSON(text)
	if v == nil {
		return 0.0
	}
	present := make([]float64, 0, 2)
	if h, ok := dim(v, "helpfulness"); ok {
		present = append(present, h)
	}
	if a, ok := dim(v, "accuracy"); ok {
		present = append(present, a)
	}
	if len(present) == 0 {
		return 0.0
	}
	var sum float64
	for _, x := range present {
		sum += x
	}
	mean := sum / float64(len(present))
	q := mean / 5.0
	if q < 0 {
		return 0
	}
	if q > 1 {
		return 1
	}
	return q
}

// dim reads a 1–5 dimension that may arrive as a number or a numeric string.
// Returns (value, true) only when value > 0 (matches judge.rs dim()).
func dim(v map[string]any, key string) (float64, bool) {
	x, ok := v[key]
	if !ok {
		return 0, false
	}
	var n float64
	switch t := x.(type) {
	case float64:
		n = t
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		n = f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		n = f
	default:
		return 0, false
	}
	if n > 0 {
		return n, true
	}
	return 0, false
}

// extractJSON strips markdown fences and extracts the first {...} JSON object
// (matches judge.rs extract_json / backend extractJSON).
func extractJSON(text string) map[string]any {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < 0 || end < start {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(text[start:end+1]), &v); err != nil {
		return nil
	}
	return v
}
