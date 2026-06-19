package gen

import (
	"context"
	"encoding/json"
	"strings"
)

// paraphrasePair LLM-rewrites a conversation pair preserving all facts. Returns
// (prompt, response, true) on success, or ("","",false) to keep the original.
func paraphrasePair(ctx context.Context, llm LLM, model, prompt, response string) (string, string, bool) {
	user := "USER MESSAGE:\n" + prompt + "\n\nASSISTANT REPLY:\n" + response + "\n\nReturn the JSON object."
	text, err := llm.Complete(ctx, model, pairParaphraseSystem, user)
	if err != nil {
		return "", "", false
	}
	v := extractJSONObject(text)
	if v == nil {
		return "", "", false
	}
	p, _ := v["prompt"].(string)
	rsp, _ := v["response"].(string)
	p, rsp = strings.TrimSpace(p), strings.TrimSpace(rsp)
	if p == "" || rsp == "" {
		return "", "", false
	}
	return p, rsp, true
}

// paraphraseQuestion LLM-rewrites a question preserving meaning. Returns "" to
// keep the original.
func paraphraseQuestion(ctx context.Context, llm LLM, model, question string) string {
	text, err := llm.Complete(ctx, model, questionParaphraseSystem, question)
	if err != nil {
		return ""
	}
	return sanitizeParaphrase(text)
}

// extractJSONObject pulls the first {...} object out of a model reply.
func extractJSONObject(text string) map[string]any {
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
