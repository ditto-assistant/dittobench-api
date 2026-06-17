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

// Dataset is a (fresh, seeded) set of tool-calling cases.
type Dataset struct {
	Seed        int64      `json:"seed"`
	GeneratedAt string     `json:"generated_at"`
	ToolCases   []ToolCase `json:"tool_cases"`
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

// CaseScore is the score for one case.
type CaseScore struct {
	CaseID    string   `json:"case_id"`
	Category  string   `json:"category"`
	ToolScore float64  `json:"tool_score"` // 0..1
	LatencyMs int64    `json:"latency_ms"`
	Called    []string `json:"called"`
	Expected  []string `json:"expected"`
	Notes     []string `json:"notes,omitempty"`
}

// ScoreReport is the full result of scoring a run.
type ScoreReport struct {
	RunID       string      `json:"run_id"`
	GeneratedAt string      `json:"generated_at"`
	Composite   float64     `json:"composite"` // 0..1, mean tool_score
	ToolMean    float64     `json:"tool_mean"` // 0..1
	MedianMs    int64       `json:"median_ms"`
	N           int         `json:"n"`
	PerCase     []CaseScore `json:"per_case"`
}
