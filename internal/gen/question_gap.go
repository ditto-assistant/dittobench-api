package gen

import (
	"context"
	"strings"
)

// questionGapSystem instructs the generator to reword a benchmark question so it
// shares as few words as possible with a memory snippet, WITHOUT answering it and
// WITHOUT changing what is asked. This is the query side of the NoLiMa
// literal-match mitigation: the answer key is unchanged, but a
// retriever can no longer key on wording shared between the question and the
// stored fact.
const questionGapSystem = "You rewrite a single benchmark question so it shares as FEW words as possible with a given MEMORY SNIPPET, while preserving its exact meaning and what it asks for. " +
	"Use synonyms and a different sentence shape; keep any concrete entity or number the question already contains. " +
	"Do NOT answer the question and do NOT include any information from the snippet's answer. " +
	"Return ONLY the rewritten question, no quotes, no preamble."

// realizeQuestionLowOverlap rewrites question to reduce its content-word overlap
// with evidenceText (the rendered stored fact). It reuses the retry-once + JSON/
// number-preservation machinery and ACCEPTS the rewrite only if it (a) is
// non-empty, (b) does not leak the canonical answer, (c) preserves any numbers,
// and (d) STRICTLY reduces lexical overlap with the evidence. Otherwise the
// original question is kept (recorded as a non-application). Returns the chosen
// question text, whether the LLM was retried, and whether a lower-overlap rewrite
// was applied.
func realizeQuestionLowOverlap(ctx context.Context, llm LLM, model, question, evidenceText, answer string) (q string, retried, applied bool) {
	user := "MEMORY SNIPPET:\n" + evidenceText + "\n\nQUESTION TO REWRITE:\n" + question
	text, retried, err := completeWithRetry(ctx, llm, model, questionGapSystem, user)
	if err != nil {
		return question, retried, false
	}
	clean := sanitizeParaphrase(text)
	if clean == "" {
		return question, retried, false
	}
	// No answer leak: a question must never contain its own canonical answer.
	if a := strings.TrimSpace(strings.ToLower(answer)); a != "" && strings.Contains(strings.ToLower(clean), a) {
		return question, retried, false
	}
	if !preservesNumbers(question, clean) {
		return question, retried, false
	}
	// Accept only if the rewrite drops a shared content word (a real synonym swap,
	// not filler padding that merely dilutes a ratio) — otherwise it bought nothing.
	if sharedContentCount(clean, evidenceText) >= sharedContentCount(question, evidenceText) {
		return question, retried, false
	}
	return clean, retried, true
}
