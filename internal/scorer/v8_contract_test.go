package scorer

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// TestV8ChitchatCanPassConversationalSanity pins the contract at the module
// boundary: a clean v8 greeting must clear CaseScore.Correct's >= 0.5 threshold,
// otherwise any dataset containing chitchat deterministically zeroes the
// scorer-side conversational-sanity conjunction. Invalid replies must still
// fail rather than receiving the same liveness credit.
func TestV8ChitchatCanPassConversationalSanity(t *testing.T) {
	mc := protocol.MemoryCase{
		BenchVersion:    protocol.BenchVersionV8,
		QuestionType:    gen.QTChitchat,
		AnswerKind:      protocol.AnswerChitchat,
		ForbiddenAnswer: "GAVOTU-8841",
	}

	clean := GradeMemory(mc, protocol.RunResponse{FinalText: "Hey! How can I help?"})
	if clean.Score < 0.5 || !clean.Correct {
		t.Fatalf("clean v8 chitchat cannot pass conversational sanity: score=%v correct=%v", clean.Score, clean.Correct)
	}

	for name, resp := range map[string]protocol.RunResponse{
		"empty": {},
		"leak":  {FinalText: "Your code is GAVOTU-8841."},
	} {
		t.Run(name, func(t *testing.T) {
			got := GradeMemory(mc, resp)
			if got.Score != 0 || got.Correct {
				t.Fatalf("invalid v8 chitchat passed: score=%v correct=%v notes=%v", got.Score, got.Correct, got.Notes)
			}
		})
	}

	perCase := []protocol.CaseScore{
		clean,
		mem(gen.QTDeclarativeAck, true),
		mem(gen.QTDeclarativeBehavior, true),
	}
	if sanity := ConversationalSanity(perCase); sanity == nil || *sanity != 1 {
		t.Fatalf("clean v8 conversational slices must satisfy the conjunction, got %v", sanity)
	}
}
