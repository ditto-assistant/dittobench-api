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
	// ForbiddenAnswer is the conflicting value another user's memory graph holds
	// for this attribute on an isolation case: the queried user's own answer is
	// ExpectedAnswer, and a response that instead surfaces ForbiddenAnswer has
	// leaked across graphs and is scored 0. Empty for ordinary cases. Like
	// ExpectedAnswer it is validator-internal grading data and is never sent to
	// the harness (only Question is).
	ForbiddenAnswer string `json:"forbidden_answer,omitempty"`
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
//
// Wave (Tier C) is the 0-based index of a STAGED seeding wave:
// the validator may call /seed repeatedly, each call carrying the next chunk of
// the haystack with an incremented Wave, and interleave /run questions between
// waves so memory is built incrementally "as you converse". Repeated /seed is an
// idempotent upsert (the reference harness's contract); a single-wave run leaves
// Wave=0. The field is ADDITIVE-OPTIONAL — a harness that ignores it and simply
// upserts each call still scores correctly.
type SeedRequest struct {
	UserID string `json:"user_id,omitempty"`
	Wave   int    `json:"wave,omitempty"`
	// omitempty so a wave with no subjects/links serializes the key as absent
	// rather than JSON null: a strict harness decoder (e.g. serde) rejects an
	// explicit null for a sequence field but accepts an absent key as empty.
	Pairs    []MemoryPair  `json:"pairs,omitempty"`
	Subjects []Subject     `json:"subjects,omitempty"`
	Links    []SubjectLink `json:"links,omitempty"`
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
//
// ToolEndpoint (Phase C) is an OPTIONAL validator-served mock
// tool-execution URL. When present, a harness that supports observed execution
// should EXECUTE its non-memory catalog tool calls by POSTing a ToolExecRequest
// to this URL (instead of stubbing them locally) and use the returned
// ToolExecResponse.Result. Doing so lets the validator (a) OBSERVE the real tool
// trajectory rather than trusting the harness's self-reported tool_calls, and
// (b) score whether the answer incorporates the returned content
// (result-usage). The field is ADDITIVE-OPTIONAL: a harness that ignores it and
// stubs tools locally still scores, but selection-only and at a capped ceiling on
// the categories the endpoint would have served (their self-reported calls are
// untrusted). Memory tools are NOT served here — the harness answers those from
// its own seeded memory.
//
// UserID (multi-graph isolation) scopes the case to one
// seeded memory graph; it mirrors the user_id the haystack was seeded under. A
// harness must answer only from that user's memory and never leak another user's
// facts. Empty means the default single-user graph ("miner").
type RunRequest struct {
	CaseID       string           `json:"case_id"`
	SystemPrompt string           `json:"system_prompt"`
	UserInput    string           `json:"user_input"`
	Tools        []ToolDefinition `json:"tools"`
	ToolEndpoint string           `json:"tool_endpoint,omitempty"`
	UserID       string           `json:"user_id,omitempty"`
}

// ToolExecRequest is what a harness POSTs to the validator-served tool_endpoint
// (RunRequest.ToolEndpoint) to actually EXECUTE one non-memory catalog tool
// during a case. The validator returns a deterministic, seed-derived mock result
// (ToolExecResponse) and records the call as the authoritative observed
// trajectory for that case (Phase C). CaseID ties the call to the
// running case; UserID echoes RunRequest.UserID; Hop is the 0-based position in
// the harness's tool sequence (for order scoring).
type ToolExecRequest struct {
	CaseID string          `json:"case_id"`
	UserID string          `json:"user_id,omitempty"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"`
	Hop    int             `json:"hop,omitempty"`
}

// ToolExecResponse is the mock result the validator returns for a ToolExecRequest.
// Result is the tool's output the harness should reason over (a web snippet, a
// page's text, a job status, …), seeded deterministically per case. Error is set
// (with Result empty) when the call is malformed or names a tool the mock server
// does not serve; a harness should treat it like a real tool error.
type ToolExecResponse struct {
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
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
	// ResultUsage is 0..1 for a result-usage tool case (Phase C): whether
	// the final answer incorporated the distinctive value the executed tool
	// returned. It replaces the LLM quality judge for those cases (deterministic).
	ResultUsage float64 `json:"result_usage,omitempty"`
	Correct     bool    `json:"correct,omitempty"` // memory judge verdict (memory cases)
	LatencyMs   int64   `json:"latency_ms"`
	// Observed is true when the harness routed this tool case's calls through the
	// validator's mock endpoint, so Called is the authoritative observed
	// trajectory (not self-report). Only observed cases feed the efficiency term.
	Observed bool     `json:"observed,omitempty"`
	Called   []string `json:"called"`
	Expected []string `json:"expected"`
	Notes    []string `json:"notes,omitempty"`
	// Injection is true when a judge flagged the harness output as an attempt to
	// manipulate the judge (prompt injection). Such a case is scored 0 and the
	// run is flagged for moderation review.
	Injection bool `json:"injection,omitempty"`
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

// ParaphraseStats counts the outcomes of the surface-realization (LLM
// paraphrase) pass for one generation. It exists so a spike in template
// fallbacks — an LLM outage or an over-strict verifier silently collapsing the
// dataset back to verbatim templates — is visible in the report
// rather than invisible. Purely advisory telemetry; never affects the score.
type ParaphraseStats struct {
	Attempted int `json:"attempted"` // paraphrase was attempted (frac roll hit, LLM present)
	Applied   int `json:"applied"`   // paraphrase verified (fact/entity preserved) and used
	Retried   int `json:"retried"`   // first LLM call failed; a second was made
	Fallback  int `json:"fallback"`  // kept the template/verbatim original (LLM error or failed verify)
}

// Add folds another ParaphraseStats into the receiver (tool + memory passes).
func (p *ParaphraseStats) Add(o ParaphraseStats) {
	p.Attempted += o.Attempted
	p.Applied += o.Applied
	p.Retried += o.Retried
	p.Fallback += o.Fallback
}

// LexicalGapStats reports the query↔needle content-word overlap of the memory
// suite — the NoLiMa literal-match signal. A question
// that shares wording with its stored fact can be answered by lexical shortcut,
// overstating memory ability; the generator rewords questions to reduce overlap
// and this makes the residual visible. Purely advisory telemetry; never scored.
type LexicalGapStats struct {
	Questions  int     `json:"questions"`   // non-abstention questions measured
	Rewritten  int     `json:"rewritten"`   // low-overlap rewrite applied
	MeanBefore float64 `json:"mean_before"` // mean question↔evidence content overlap, original text
	MeanAfter  float64 `json:"mean_after"`  // ... after rewrite (or original where not rewritten)
}

// RunDetails is the opaque, additive telemetry blob for a run.
// It is NOT part of the platform's DB/signature contract, so new fields may be
// added freely (later work adds bench_version, judge-audit stats, token totals).
// Serialized under ScoreReport.details.
type RunDetails struct {
	// BenchVersion is the scoring benchmark version (see protocol.BenchVersion).
	// The weight fold only compares entries of the max bench_version present, so a
	// bump makes new scores non-comparable to old until a re-score.
	BenchVersion int `json:"bench_version"`
	// DatasetSHA256 is the hex SHA-256 of the fully-rendered dataset (tool cases +
	// memory waves + memory cases). It pins the exact artifact a dispute re-scores
	// re-scores: the recorded hash must match a re-hash of the persisted
	// artifact. With no LLM surface variation it is also reproducible from
	// (seed, bench_version).
	DatasetSHA256 string           `json:"dataset_sha256,omitempty"`
	Paraphrase    *ParaphraseStats `json:"paraphrase,omitempty"`
	// InjectionAttempts counts cases a judge flagged as judge-manipulation
	// attempts (each scored 0). A non-zero value is moderation-relevant evidence,
	// the same policy channel as plagiarism.
	InjectionAttempts int `json:"injection_attempts,omitempty"`
	// Tokens is the total OpenRouter tokens (generator + judge) the run spent —
	// budget telemetry (kept out of the composite).
	Tokens int64 `json:"tokens,omitempty"`
	// SeedingWaves is how many staged /seed waves the memory haystack was split
	// into (Tier C; 1 = single seed). RawPairsCases is how many memory cases were
	// Tier B (raw-pairs seeding: their evidence was seeded WITHOUT prepared
	// subjects, so the harness had to build its own subject index). Both
	// are advisory calibration telemetry.
	SeedingWaves  int `json:"seeding_waves,omitempty"`
	RawPairsCases int `json:"raw_pairs_cases,omitempty"`
	// ToolMean / MemoryMean echo the per-suite means for convenience alongside the
	// per-category breakdown in ScoreReport.per_category.
	ToolMean   float64 `json:"tool_mean"`
	MemoryMean float64 `json:"memory_mean"`
	// LexicalGap is the query↔needle overlap telemetry for the memory suite (the
	// NoLiMa literal-match signal). Advisory only.
	LexicalGap *LexicalGapStats `json:"lexical_gap,omitempty"`
	// ObservedToolCases is how many tool cases were scored on the validator-
	// observed trajectory (the harness routed its calls through tool_endpoint);
	// CappedToolCases is how many observable cases were capped because the harness
	// did NOT (self-report untrusted). Together they show how much of
	// the tool suite ran under observed execution. Advisory calibration telemetry.
	ObservedToolCases int `json:"observed_tool_cases,omitempty"`
	CappedToolCases   int `json:"capped_tool_cases,omitempty"`
	// IsolationCases is how many multi-graph isolation cases ran — a second
	// persona seeded under a different user_id with a conflicting value, so a
	// cross-graph memory leak scores wrong. Advisory telemetry.
	IsolationCases int `json:"isolation_cases,omitempty"`
	// ToolEfficiency is the run's observed tool-efficiency factor (0..1) folded
	// into the composite as a bounded multiplier: 1.0 when harnesses reached
	// correct answers within their expected tool budget, dropping toward the floor
	// as observed trajectories overshot. 1.0 (no effect) when no tool case ran
	// under observed execution. Advisory — the composite already reflects it.
	ToolEfficiency float64 `json:"tool_efficiency,omitempty"`
	// Models records the LLM model ids that produced this run — the datagen
	// generator, the judge(s), and (when the operator forces it) the miner's
	// harness chat model. Advisory transparency metadata for the public
	// leaderboard: it makes a composite reproducible/comparable (a score is only
	// meaningful alongside the models that produced the dataset + graded it).
	Models *ModelInfo `json:"models,omitempty"`
	// PerCategory echoes ScoreReport.PerCategory into the details blob so the
	// per-category breakdown (per tool / per memory-question-type mean) survives
	// the platform wire — which carries details but not the top-level
	// per_category — and can drive a transparent leaderboard. Advisory only.
	PerCategory []CategoryStat `json:"per_category,omitempty"`
}

// ModelInfo is the set of LLM model ids a run was produced with (RunDetails.models).
// All fields are advisory transparency metadata — never scored or signed.
type ModelInfo struct {
	// Generator is the datagen model (paraphrase + haystack surface realization).
	Generator string `json:"generator,omitempty"`
	// Judge is the primary scoring/judge model.
	Judge string `json:"judge,omitempty"`
	// JudgeAudit is the optional second judge model used for the audit ensemble
	// (empty when SCORER_MODEL_B is unset).
	JudgeAudit string `json:"judge_audit,omitempty"`
	// Harness is the miner harness's chat model when the operator forces it via
	// DITTOBENCH_HARNESS_MODEL; empty when the harness used its own default (the
	// server does not otherwise observe the miner's model choice).
	Harness string `json:"harness,omitempty"`
}

// ScoreReport is the full result of scoring a run.
type ScoreReport struct {
	RunID       string         `json:"run_id"`
	Seed        int64          `json:"seed"` // dataset seed (anti-overfit reproducibility)
	GeneratedAt string         `json:"generated_at"`
	Composite   float64        `json:"composite"`   // 0..1 weighted composite: 0.5*tool_mean + 0.5*memory_mean (v2)
	ToolMean    float64        `json:"tool_mean"`   // 0..1 mean tool-case composite
	MemoryMean  float64        `json:"memory_mean"` // 0..1 fraction of memory cases correct
	MedianMs    int64          `json:"median_ms"`
	N           int            `json:"n"`
	PerCase     []CaseScore    `json:"per_case"`
	PerCategory []CategoryStat `json:"per_category,omitempty"`
	// Details is opaque, additive run telemetry (paraphrase fallback counts and,
	// in later bench versions, more). Advisory only — never scored or signed.
	Details *RunDetails `json:"details,omitempty"`
	// StructuralFingerprint is an AST-level shingle sketch of the built crate
	// (nil when unavailable), forwarded to the platform's anti-copy gate as
	// advisory (unsigned) moderation metadata. It never affects the score.
	StructuralFingerprint *CodeFingerprint `json:"structural_fingerprint,omitempty"`
}
