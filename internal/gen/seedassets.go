package gen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Env var names + defaults for the LongMemEval seed assets.
const (
	EnvSeedDir = "DITTOBENCH_SEED_DIR"
	EnvOracle  = "DITTOBENCH_ORACLE"

	defaultSeedDir = "/Users/omarbarazanji/omar-workspace/ditto-backend-ops-log/longmemeval"
	defaultOracle  = "/Users/omarbarazanji/omar-workspace/ditto-backend-ops-log/dittobench-testdata/longmemeval/longmemeval_oracle.json"
)

// SeedDir returns the seed-asset directory (env DITTOBENCH_SEED_DIR or default).
func SeedDir() string {
	if d := os.Getenv(EnvSeedDir); d != "" {
		return d
	}
	return defaultSeedDir
}

// OraclePath returns the oracle file path (env DITTOBENCH_ORACLE or default).
func OraclePath() string {
	if p := os.Getenv(EnvOracle); p != "" {
		return p
	}
	return defaultOracle
}

// --- on-disk shapes (embeddings intentionally ignored) ---

type seedPair struct {
	PairID   string `json:"pair_id"`
	Prompt   string `json:"prompt"`
	Response string `json:"response"`
	// embedding, timestamp, case_id ignored (we assign fresh timestamps)
}

type seedSubject struct {
	ID              string `json:"id"`
	SubjectText     string `json:"subject_text"`
	DescriptionText string `json:"description_text"`
	// subject_type, embedding ignored
}

type seedLink struct {
	SubjectID       string `json:"subject_id"`
	FirestorePairID string `json:"firestore_pair_id"`
}

type seedManifest struct {
	FixtureUser string         `json:"fixture_user"`
	Cases       []manifestCase `json:"cases"`
}

type manifestCase struct {
	QuestionID     string              `json:"question_id"`
	SessionToPairs map[string][]string `json:"session_to_pairs"`
	PairCount      int                 `json:"pair_count"`
}

type oracleQuestion struct {
	QuestionID   string          `json:"question_id"`
	QuestionType string          `json:"question_type"`
	Question     string          `json:"question"`
	Answer       json.RawMessage `json:"answer"` // may be string or number
}

// seedAssets is the parsed corpus needed to build a fresh haystack.
type seedAssets struct {
	pairsByID    map[string]seedPair
	subjectsByID map[string]seedSubject
	linksByPair  map[string][]string // pair_id -> []subject_id
	manifest     map[string]manifestCase
	oracle       map[string]oracleQuestion
	oracleOrder  []string // stable order of oracle question_ids
}

// loadSeedAssets reads + parses the seed dir + oracle. Returns a clear error if
// any file is missing (so generation errors instead of crashing).
func loadSeedAssets(seedDir, oraclePath string) (*seedAssets, error) {
	if fi, err := os.Stat(seedDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("seed dir %q not found (set %s)", seedDir, EnvSeedDir)
	}
	if _, err := os.Stat(oraclePath); err != nil {
		return nil, fmt.Errorf("oracle %q not found (set %s)", oraclePath, EnvOracle)
	}

	var pairs []seedPair
	if err := readJSON(filepath.Join(seedDir, "seed_pairs.json"), &pairs); err != nil {
		return nil, err
	}
	var subjects []seedSubject
	if err := readJSON(filepath.Join(seedDir, "seed_subjects.json"), &subjects); err != nil {
		return nil, err
	}
	var links []seedLink
	if err := readJSON(filepath.Join(seedDir, "seed_subject_links.json"), &links); err != nil {
		return nil, err
	}
	var manifest seedManifest
	if err := readJSON(filepath.Join(seedDir, "seed_manifest.json"), &manifest); err != nil {
		return nil, err
	}
	var oracle []oracleQuestion
	if err := readJSON(oraclePath, &oracle); err != nil {
		return nil, err
	}

	a := &seedAssets{
		pairsByID:    make(map[string]seedPair, len(pairs)),
		subjectsByID: make(map[string]seedSubject, len(subjects)),
		linksByPair:  make(map[string][]string),
		manifest:     make(map[string]manifestCase, len(manifest.Cases)),
		oracle:       make(map[string]oracleQuestion, len(oracle)),
	}
	for _, p := range pairs {
		a.pairsByID[p.PairID] = p
	}
	for _, s := range subjects {
		a.subjectsByID[s.ID] = s
	}
	for _, l := range links {
		a.linksByPair[l.FirestorePairID] = append(a.linksByPair[l.FirestorePairID], l.SubjectID)
	}
	for _, c := range manifest.Cases {
		a.manifest[c.QuestionID] = c
	}
	for _, q := range oracle {
		if _, dup := a.oracle[q.QuestionID]; !dup {
			a.oracleOrder = append(a.oracleOrder, q.QuestionID)
		}
		a.oracle[q.QuestionID] = q
	}
	if len(a.oracleOrder) == 0 {
		return nil, fmt.Errorf("oracle %q has no questions", oraclePath)
	}
	return a, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return nil
}

// answerText renders an oracle answer (string or number) as plain text.
func answerText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// number / other JSON scalar: use the raw form trimmed of surrounding space.
	return string(raw)
}
