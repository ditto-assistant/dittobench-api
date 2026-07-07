package main

import (
	"context"
	"fmt"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-api/internal/persona"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// Judge-adjacency audit (BENCHMARK-V2 review §8.2). LoCoMo's judge accepted
// 62.81% of intentionally wrong-but-topically-adjacent answers, so a benchmark
// whose key is sound can still be corrupted by a lenient judge. This audit feeds
// the REAL judge three response classes per derived question and checks it:
//
//   - CORRECT     (the true answer, or a grounded decline for abstention) → accept
//   - ADJACENT    (a sibling value of the same type — plausible but wrong)  → reject
//   - OFF-TOPIC   (an unrelated answer)                                     → reject
//
// The adjacent + off-topic classes always MISS the deterministic containment
// check, so they genuinely exercise the LLM judge. It reports accept / reject
// rates and passes only if the judge is both sensitive (accepts truth) and
// specific (rejects near-misses) above threshold.

// JudgeAuditReport is the audit outcome.
type JudgeAuditReport struct {
	Model            string  `json:"model"`
	Cases            int     `json:"cases"`
	AcceptRate       float64 `json:"accept_rate"`         // correct answers judged correct (sensitivity)
	AdjacentReject   float64 `json:"adjacent_reject_rate"` // wrong-adjacent judged wrong (the LoCoMo failure)
	OffTopicReject   float64 `json:"offtopic_reject_rate"`
	JudgeCalls       int     `json:"judge_calls"` // responses that reached the LLM judge (deterministic miss)
	AcceptThreshold  float64 `json:"accept_threshold"`
	RejectThreshold  float64 `json:"reject_threshold"`
	Pass             bool    `json:"pass"`
}

const offTopicResp = "I don't think that's something we ever went over; let's move on to something else."

// judgeAudit builds correct/adjacent/off-topic responses from derived questions
// of two independent personas and grades each with the real judge.
func judgeAudit(nSeeds int) (JudgeAuditReport, error) {
	client, err := llm.New()
	if err != nil {
		return JudgeAuditReport{}, err
	}
	cfg := scorer.JudgeConfig{Model: llm.ScorerModel(), ModelB: llm.ScorerModelB()}
	ctx := context.Background()

	var cases, accepts, adjRejects, otRejects, adjN, otN, judgeCalls int

	for s := 0; s < nSeeds; s++ {
		planA := persona.BuildPlan(int64(2*s+1), persona.DefaultOpts())
		planB := persona.BuildPlan(int64(2*s+2), persona.DefaultOpts())
		// Index B's non-abstention questions by type, for same-type siblings.
		siblingByType := map[string]persona.Question{}
		for _, q := range persona.DeriveQuestions(planB) {
			if !q.Abstain && q.Answer != "" {
				siblingByType[q.Type] = q
			}
		}

		for _, q := range persona.DeriveQuestions(planA) {
			// --- CORRECT / decline ---
			if q.Abstain {
				cs, jo := grade(ctx, client, cfg, q, "You never mentioned that, so I don't have it on record — I'd only be guessing.")
				cases++
				if cs.Correct {
					accepts++
				}
				if jo.Attempted {
					judgeCalls++
				}
				// wrong: a confident fabrication instead of a decline.
				cs, jo = grade(ctx, client, cfg, q, "Yes, I remember — it's the one you told me about earlier.")
				otN++
				if !cs.Correct {
					otRejects++
				}
				if jo.Attempted {
					judgeCalls++
				}
				continue
			}
			if q.Answer == "" {
				continue
			}
			cs, jo := grade(ctx, client, cfg, q, "From what you've told me, it's "+q.Answer+".")
			cases++
			if cs.Correct {
				accepts++
			}
			if jo.Attempted {
				judgeCalls++
			}
			// --- ADJACENT (same-type sibling, different value) ---
			if sib, ok := siblingByType[q.Type]; ok && sib.Answer != "" && sib.Answer != q.Answer {
				cs, jo := grade(ctx, client, cfg, q, "From what you've told me, it's "+sib.Answer+".")
				adjN++
				if !cs.Correct {
					adjRejects++
				}
				if jo.Attempted {
					judgeCalls++
				}
			}
			// --- OFF-TOPIC ---
			cs, jo = grade(ctx, client, cfg, q, offTopicResp)
			otN++
			if !cs.Correct {
				otRejects++
			}
			if jo.Attempted {
				judgeCalls++
			}
		}
	}

	rep := JudgeAuditReport{
		Model:           cfg.Model,
		Cases:           cases,
		AcceptRate:      rate(accepts, cases),
		AdjacentReject:  rate(adjRejects, adjN),
		OffTopicReject:  rate(otRejects, otN),
		JudgeCalls:      judgeCalls,
		AcceptThreshold: 0.90,
		RejectThreshold: 0.85,
	}
	rep.Pass = rep.AcceptRate >= rep.AcceptThreshold &&
		rep.AdjacentReject >= rep.RejectThreshold &&
		rep.OffTopicReject >= 0.95
	return rep, nil
}

func grade(ctx context.Context, client scorer.LLM, cfg scorer.JudgeConfig, q persona.Question, response string) (protocol.CaseScore, scorer.JudgeOutcome) {
	expected := q.Answer
	if q.Abstain {
		expected = "(No information about this was ever provided to you. The correct behavior is to decline or state you do not have that information — never to fabricate an answer.)"
	}
	mc := protocol.MemoryCase{ID: q.ID, QuestionID: q.ID, QuestionType: q.Type, Question: q.Text, ExpectedAnswer: expected}
	return scorer.GradeMemory(ctx, client, cfg, mc, protocol.RunResponse{FinalText: response, LatencyMs: 1})
}

func rate(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return round4(float64(n) / float64(d))
}

func printJudgeAudit(rep JudgeAuditReport) {
	fmt.Printf("judge-adjacency audit (model=%s, %d cases, %d judge calls)\n", rep.Model, rep.Cases, rep.JudgeCalls)
	fmt.Printf("  accept (truth→correct):        %.3f  (threshold ≥ %.2f)\n", rep.AcceptRate, rep.AcceptThreshold)
	fmt.Printf("  adjacent reject (near-miss→wrong): %.3f  (threshold ≥ %.2f)  ← the LoCoMo failure mode\n", rep.AdjacentReject, rep.RejectThreshold)
	fmt.Printf("  off-topic reject:               %.3f  (threshold ≥ 0.95)\n", rep.OffTopicReject)
	if rep.Pass {
		fmt.Println("  RESULT: PASS")
	} else {
		fmt.Println("  RESULT: FAIL")
	}
}
