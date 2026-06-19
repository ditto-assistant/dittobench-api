package gen

import (
	"context"
	"math/rand"
	"strings"

	"github.com/ditto-assistant/dittobench-api/internal/datagen"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

const paraphraseSystem = "You rewrite a single user message to a chat assistant so the WORDING is different but the INTENT is identical. " +
	"Preserve every concrete entity (names, topics, URLs, themes, tasks) exactly. " +
	"Do not add or remove any request. Do not answer it. Keep it one short message, first person. " +
	"Return ONLY the rewritten message, no quotes, no preamble."

// GenerateTools builds n fresh tool-calling cases. It reuses datagen's templated
// ground truth (so expected_tools / expected_behavior stay deterministic) and
// then LLM-paraphrases a random `frac` of the prompts via the generator model so
// the surface wording is novel every run. A nil llm (or frac<=0) skips
// paraphrasing — used by cheap/offline paths and tests.
func GenerateTools(ctx context.Context, r *rand.Rand, n int, frac float64, llm LLM, model string) []protocol.ToolCase {
	cases := datagen.GenerateCases(r, r.Int63(), n)
	if llm == nil || frac <= 0 {
		return cases
	}
	for i := range cases {
		if r.Float64() >= frac {
			continue
		}
		rewritten, err := llm.Complete(ctx, model, paraphraseSystem, cases[i].Prompt)
		if err != nil {
			continue // keep the templated prompt on any failure
		}
		if clean := sanitizeParaphrase(rewritten); clean != "" {
			cases[i].Prompt = clean
		}
	}
	return cases
}

// sanitizeParaphrase trims model chatter (surrounding quotes/whitespace, leading
// "Sure," etc.). Returns "" if the result looks degenerate so the caller keeps
// the original.
func sanitizeParaphrase(s string) string {
	s = strings.TrimSpace(s)
	// Strip surrounding matched quotes.
	for _, q := range []string{`"`, "'", "`"} {
		if strings.HasPrefix(s, q) && strings.HasSuffix(s, q) && len(s) >= 2 {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	// Drop an accidental code fence.
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 600 {
		return ""
	}
	return s
}
