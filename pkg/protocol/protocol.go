// Package protocol defines the shared wire types exchanged between the
// DittoBench validator (on Bittensor subnet 118) and a miner's agent harness.
//
// These types MUST stay byte-compatible with the dittobench-api. They are
// reproduced here verbatim so miners can build and test a harness offline
// without any private dependency.
package protocol

import "encoding/json"

// ToolSpec is an expected tool in a dataset case.
type ToolSpec struct {
	Name          string            `json:"name"`
	RequiredArgs  map[string]string `json:"required_args,omitempty"`
	ForbiddenArgs []string          `json:"forbidden_args,omitempty"`
}

// ToolCase is one tool-calling benchmark case.
type ToolCase struct {
	ID               string     `json:"id"`
	Category         string     `json:"category"`
	Prompt           string     `json:"prompt"`
	ExpectedTools    []ToolSpec `json:"expected_tools"`
	MaxToolCalls     int        `json:"max_tool_calls"`
	AllowExtraTools  bool       `json:"allow_extra_tools"`
	ExpectedBehavior string     `json:"expected_behavior,omitempty"`
}

// MemoryCase is one memory-recall (LongMemEval) benchmark case. The harness is
// first seeded with a fresh haystack (see SeedRequest); then for each case the
// validator POSTs a normal RunRequest whose user_input is Question, and the
// agent must answer from its seeded memory. ExpectedAnswer is the oracle answer
// (judged for containment, not exact match).
type MemoryCase struct {
	ID             string `json:"id"`
	QuestionID     string `json:"question_id"`
	QuestionType   string `json:"question_type"`
	Question       string `json:"question"`
	ExpectedAnswer string `json:"expected_answer"`
}

// Dataset is a (fresh, seeded) set of tool-calling + memory cases.
type Dataset struct {
	Seed        int64        `json:"seed"`
	GeneratedAt string       `json:"generated_at"`
	ToolCases   []ToolCase   `json:"tool_cases"`
	MemoryCases []MemoryCase `json:"memory_cases,omitempty"`
}

// MemoryPair is one conversation pair in a fresh haystack pushed to the harness
// via POST /seed. The harness embeds prompt+response and stores it for recall.
type MemoryPair struct {
	PairID    string `json:"pair_id"`
	SessionID string `json:"session_id"`
	Timestamp string `json:"timestamp"` // RFC3339
	Prompt    string `json:"prompt"`
	Response  string `json:"response"`
}

// Subject is one subject/topic cluster linked to memory pairs in a haystack.
type Subject struct {
	ID              string `json:"id"`
	SubjectText     string `json:"subject_text"`
	DescriptionText string `json:"description_text"`
}

// SubjectLink ties a Subject to a MemoryPair (many-to-many).
type SubjectLink struct {
	SubjectID string `json:"subject_id"`
	PairID    string `json:"pair_id"`
}

// SeedRequest is the fresh haystack the validator POSTs to <harness>/seed before
// running memory cases. UserID defaults to "miner" if empty.
type SeedRequest struct {
	UserID   string        `json:"user_id,omitempty"`
	Pairs    []MemoryPair  `json:"pairs"`
	Subjects []Subject     `json:"subjects"`
	Links    []SubjectLink `json:"links"`
}

// SeedResponse is what <harness>/seed returns: counts actually loaded.
type SeedResponse struct {
	Pairs    int `json:"pairs"`
	Subjects int `json:"subjects"`
	Links    int `json:"links"`
}

// ToolDefinition is a tool schema sent to the harness for a case.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// RunRequest is what the validator POSTs to the harness /run endpoint per case.
type RunRequest struct {
	CaseID       string           `json:"case_id"`
	SystemPrompt string           `json:"system_prompt"`
	UserInput    string           `json:"user_input"`
	Tools        []ToolDefinition `json:"tools"`
}

// ObservedToolCall is a tool call the harness made.
type ObservedToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
	Hop  int             `json:"hop,omitempty"`
}

// RunResponse is what the harness returns for a case.
type RunResponse struct {
	FinalText    string             `json:"final_text"`
	ToolCalls    []ObservedToolCall `json:"tool_calls"`
	PromptTokens int64              `json:"prompt_tokens"`
	OutputTokens int64              `json:"output_tokens"`
	LatencyMs    int64              `json:"latency_ms"`
}

// Kind discriminates a CaseScore between the two case families.
const (
	KindTool   = "tool"
	KindMemory = "memory"
)

// CaseScore is the score for one case (tool OR memory).
//
// For a tool case: Score = 0.5*ToolAccuracy + 0.5*Quality (the LLM judge half).
// For a memory case: Score is 1.0 or 0.0 from the LongMemEval yes/no judge, and
// ToolAccuracy/Quality are unused.
type CaseScore struct {
	CaseID    string  `json:"case_id"`
	Category  string  `json:"category"`
	Kind      string  `json:"kind"`              // "tool" | "memory"
	Score     float64 `json:"score"`             // 0..1 composite for this case
	ToolScore float64 `json:"tool_score"`        // 0..1 deterministic tool accuracy (tool cases)
	Quality   float64 `json:"quality,omitempty"` // 0..1 LLM response-quality judge (tool cases)
	Correct   bool    `json:"correct,omitempty"` // memory judge verdict (memory cases)
	LatencyMs int64   `json:"latency_ms"`
	// LatencyScore is the 0..1 wall-clock reward for this case: 1.0 at/below the
	// latency target, 0.0 at/above the ceiling, linear between (see scorer).
	LatencyScore float64  `json:"latency_score"`
	Called       []string `json:"called"`
	Expected     []string `json:"expected"`
	Notes        []string `json:"notes,omitempty"`
}

// CategoryStat is the mean composite score for one category.
type CategoryStat struct {
	Category string  `json:"category"`
	Count    int     `json:"count"`
	Mean     float64 `json:"mean"`
}

// CodeFingerprint is a bottom-k MinHash (KMV) sketch of a submission's source,
// consumed by the platform's anti-copy moderation gate. It is advisory metadata,
// never part of the scored result: V is the sketch-format version, K the bottom-k
// budget, Card the true shingle-set cardinality, and M the sorted bottom-K shingle
// hashes. The shape is byte-compatible with the platform's own fingerprint sketch
// so the two compare with one code path (Jaccard / containment over M).
type CodeFingerprint struct {
	V    int      `json:"v"`
	K    int      `json:"k"`
	Card int      `json:"card"`
	M    []string `json:"m"`
}

// ScoreReport is the full result of scoring a run.
type ScoreReport struct {
	RunID       string  `json:"run_id"`
	Seed        int64   `json:"seed"` // dataset seed (anti-overfit reproducibility)
	GeneratedAt string  `json:"generated_at"`
	Composite   float64 `json:"composite"`   // 0..1 weighted composite: (1-latency_weight)*(0.6*tool_mean+0.4*memory_mean) + latency_weight*latency_mean
	ToolMean    float64 `json:"tool_mean"`   // 0..1 mean tool-case composite
	MemoryMean  float64 `json:"memory_mean"` // 0..1 fraction of memory cases correct
	// LatencyMean is the 0..1 mean per-case wall-clock reward (see scorer's
	// latency curve). MedianMs is the raw median latency in ms, kept for display.
	LatencyMean float64        `json:"latency_mean"`
	MedianMs    int64          `json:"median_ms"`
	N           int            `json:"n"`
	PerCase     []CaseScore    `json:"per_case"`
	PerCategory []CategoryStat `json:"per_category,omitempty"`
	// StructuralFingerprint is an AST-level shingle sketch of the built crate
	// (nil when unavailable), forwarded to the platform's anti-copy gate as
	// advisory (unsigned) moderation metadata. It never affects the score.
	StructuralFingerprint *CodeFingerprint `json:"structural_fingerprint,omitempty"`
}
