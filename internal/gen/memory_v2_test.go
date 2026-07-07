package gen

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/persona"
)

func TestGenerateMemoryV2Deterministic(t *testing.T) {
	seedA, caseA, _, err := GenerateMemoryV2(context.Background(), NewRNG(5), 12345, 20, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	seedB, caseB, _, err := GenerateMemoryV2(context.Background(), NewRNG(5), 12345, 20, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(struct {
		S any
		C any
	}{seedA, caseA})
	jb, _ := json.Marshal(struct {
		S any
		C any
	}{seedB, caseB})
	if string(ja) != string(jb) {
		t.Fatal("GenerateMemoryV2 not reproducible for fixed (seed, rng stream)")
	}
}

func TestGenerateMemoryV2Shape(t *testing.T) {
	seedReq, cases, _, err := GenerateMemoryV2(context.Background(), NewRNG(1), 999, 30, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(seedReq.Pairs) == 0 {
		t.Fatal("empty haystack")
	}
	if len(seedReq.Subjects) == 0 || len(seedReq.Links) == 0 {
		t.Fatal("Tier-A seeding should synthesize subjects + links")
	}
	if len(cases) == 0 {
		t.Fatal("no memory cases")
	}
	// Abstention share present, every case has a non-empty question + answer.
	var absN int
	types := map[string]int{}
	for _, c := range cases {
		types[c.QuestionType]++
		if strings.TrimSpace(c.Question) == "" || strings.TrimSpace(c.ExpectedAnswer) == "" {
			t.Fatalf("case %s has empty question or answer", c.ID)
		}
		if strings.Contains(c.QuestionType, "abstention") {
			absN++
		}
	}
	if absN == 0 {
		t.Error("no abstention cases at n=30")
	}
	if len(types) < 4 {
		t.Errorf("expected several question types, got %v", types)
	}
}

// TestGenerateMemoryV2AnswersAreSeeded checks the recall/knowledge-update/
// preference answers are actually present in the seeded haystack (the harness
// can recover them), while abstention answers are the decline sentinel and are
// NOT seeded.
func TestGenerateMemoryV2AnswersAreSeeded(t *testing.T) {
	seedReq, cases, _, err := GenerateMemoryV2(context.Background(), NewRNG(2), 42, 25, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, p := range seedReq.Pairs {
		sb.WriteString(p.Prompt)
		sb.WriteByte(' ')
		sb.WriteString(p.Response)
		sb.WriteByte(' ')
	}
	hay := strings.ToLower(sb.String())
	for _, c := range cases {
		if strings.Contains(c.QuestionType, "abstention") {
			if c.ExpectedAnswer != abstentionExpectedAnswer {
				t.Fatalf("abstention case %s answer is not the decline sentinel", c.ID)
			}
			continue
		}
		switch c.QuestionType {
		case persona.QTSingleSession, persona.QTKnowledgeUpdate, persona.QTPreference:
			if !strings.Contains(hay, strings.ToLower(c.ExpectedAnswer)) {
				t.Fatalf("case %s (%s): answer %q not present in seeded haystack",
					c.ID, c.QuestionType, c.ExpectedAnswer)
			}
		}
	}
}
