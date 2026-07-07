package gen

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// completeWithRetry calls the generator model once, retrying a single time on
// error (transient LLM/availability blips are common). The bool reports whether
// the retry was used, so the caller can count it in ParaphraseStats.
func completeWithRetry(ctx context.Context, llm LLM, model, system, user string) (string, bool, error) {
	text, err := llm.Complete(ctx, model, system, user)
	if err == nil {
		return text, false, nil
	}
	text, err = llm.Complete(ctx, model, system, user)
	return text, true, err
}

// paraphrasePair LLM-rewrites a conversation pair preserving all facts. Retries
// once on LLM error and verifies the rewrite before accepting it: any number or
// date in the original must survive verbatim (paraphrase must not corrupt the
// fact being tested). Returns (prompt, response, retried, ok); ok=false means
// the caller keeps the original and records a fallback — never a silent skip.
func paraphrasePair(ctx context.Context, llm LLM, model, prompt, response string) (p, rsp string, retried, ok bool) {
	user := "USER MESSAGE:\n" + prompt + "\n\nASSISTANT REPLY:\n" + response + "\n\nReturn the JSON object."
	text, retried, err := completeWithRetry(ctx, llm, model, pairParaphraseSystem, user)
	if err != nil {
		return "", "", retried, false
	}
	v := extractJSONObject(text)
	if v == nil {
		return "", "", retried, false
	}
	p, _ = v["prompt"].(string)
	rsp, _ = v["response"].(string)
	p, rsp = strings.TrimSpace(p), strings.TrimSpace(rsp)
	if p == "" || rsp == "" {
		return "", "", retried, false
	}
	// Fact check: every number/date in the original pair must survive so the
	// paraphrase cannot silently alter the answerable content.
	if !preservesNumbers(prompt+" "+response, p+" "+rsp) {
		return "", "", retried, false
	}
	return p, rsp, retried, true
}

// paraphraseQuestion LLM-rewrites a question preserving meaning. Retries once on
// LLM error and verifies numbers/dates survive. Returns (question, retried, ok);
// ok=false means keep the original (recorded as a fallback).
func paraphraseQuestion(ctx context.Context, llm LLM, model, question string) (q string, retried, ok bool) {
	text, retried, err := completeWithRetry(ctx, llm, model, questionParaphraseSystem, question)
	if err != nil {
		return "", retried, false
	}
	clean := sanitizeParaphrase(text)
	if clean == "" || !preservesNumbers(question, clean) {
		return "", retried, false
	}
	return clean, retried, true
}

var numberRe = regexp.MustCompile(`[0-9]+`)

// preservesNumbers reports whether every digit run in orig also appears in
// rewritten. Numbers and dates are the fact-critical, objectively checkable
// tokens a paraphrase must never drop or mutate; softer entities (names, topics)
// are left to the model instruction and, in Phase B, the persona-plan canonical
// check that extends this machinery.
func preservesNumbers(orig, rewritten string) bool {
	for _, n := range numberRe.FindAllString(orig, -1) {
		if !strings.Contains(rewritten, n) {
			return false
		}
	}
	return true
}

// entityStopwords are dropped from a tool filler before the content-word check
// so a legitimate rewording ("my flight to Tokyo" -> "the Tokyo flight I have")
// still passes as long as the concrete words (flight, Tokyo) survive.
var entityStopwords = map[string]bool{
	"my": true, "the": true, "a": true, "an": true, "to": true, "of": true,
	"for": true, "on": true, "in": true, "is": true, "it": true, "me": true,
	"i": true, "and": true, "or": true, "vs": true, "that": true, "this": true,
}

var wordRe = regexp.MustCompile(`[a-z0-9]+`)

// preservesEntity reports whether the concrete entity of a tool case survives a
// paraphrase. A URL must appear verbatim; otherwise every content word (>=3
// chars or numeric, minus stopwords) of the filler must appear in the rewrite.
func preservesEntity(filler, rewritten string) bool {
	lr := strings.ToLower(rewritten)
	lf := strings.ToLower(strings.TrimSpace(filler))
	if strings.HasPrefix(lf, "http") {
		return strings.Contains(lr, lf)
	}
	for _, tok := range wordRe.FindAllString(lf, -1) {
		if entityStopwords[tok] || (len(tok) < 3 && !isNumeric(tok)) {
			continue
		}
		if !strings.Contains(lr, tok) {
			return false
		}
	}
	return true
}

func isNumeric(s string) bool { return numberRe.MatchString(s) && wordRe.FindString(s) == s }

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
