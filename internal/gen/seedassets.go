package gen

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Env var names for overriding the LongMemEval seed assets with on-disk copies.
// When unset, the validator uses the embedded slim bundle (see seeddata/), so a
// hosted deploy needs no external files — this is what makes the cloud practice
// API self-contained.
const (
	EnvSeedDir = "DITTOBENCH_SEED_DIR"
	EnvOracle  = "DITTOBENCH_ORACLE"
)

// embeddedSeeds is the slim, text-only LongMemEval bundle (embeddings stripped):
// conversation pairs, subjects, the subject↔pair graph, the manifest, and the
// ~500-question oracle. ~18MB, baked into the binary so the service is portable.
//
//go:embed seeddata
var embeddedSeeds embed.FS

const embeddedRoot = "seeddata"

// SeedDir returns the on-disk seed-asset dir from env DITTOBENCH_SEED_DIR, or ""
// to use the embedded bundle.
func SeedDir() string { return os.Getenv(EnvSeedDir) }

// OraclePath returns the on-disk oracle path from env DITTOBENCH_ORACLE, or ""
// to use the embedded bundle.
func OraclePath() string { return os.Getenv(EnvOracle) }

// assetCache memoizes parsed seed bundles keyed by source so the ~18MB embedded
// (or larger on-disk) corpus is parsed once, not per submission.
var (
	assetMu    sync.Mutex
	assetCache = map[string]*seedAssets{}
)

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

// loadSeedAssets reads + parses the seed corpus. When seedDir/oraclePath are
// empty it reads the embedded bundle (the hosted default); when set it reads
// the on-disk copies (local dev with the full assets). Results are cached per
// source. Returns a clear error if an on-disk file is missing (so generation
// errors instead of crashing).
func loadSeedAssets(seedDir, oraclePath string) (*seedAssets, error) {
	cacheKey := seedDir + "|" + oraclePath
	assetMu.Lock()
	defer assetMu.Unlock()
	if a, ok := assetCache[cacheKey]; ok {
		return a, nil
	}

	// On-disk overrides are validated up front for a clear error message.
	if seedDir != "" {
		if fi, err := os.Stat(seedDir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("seed dir %q not found (set %s)", seedDir, EnvSeedDir)
		}
	}
	if oraclePath != "" {
		if _, err := os.Stat(oraclePath); err != nil {
			return nil, fmt.Errorf("oracle %q not found (set %s)", oraclePath, EnvOracle)
		}
	}

	var pairs []seedPair
	if err := readAsset(seedDir, "seed_pairs.json", &pairs); err != nil {
		return nil, err
	}
	var subjects []seedSubject
	if err := readAsset(seedDir, "seed_subjects.json", &subjects); err != nil {
		return nil, err
	}
	var links []seedLink
	if err := readAsset(seedDir, "seed_subject_links.json", &links); err != nil {
		return nil, err
	}
	var manifest seedManifest
	if err := readAsset(seedDir, "seed_manifest.json", &manifest); err != nil {
		return nil, err
	}
	var oracle []oracleQuestion
	if err := readOracle(oraclePath, &oracle); err != nil {
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
		return nil, fmt.Errorf("oracle has no questions")
	}
	assetCache[cacheKey] = a
	return a, nil
}

// readAsset reads one seed-dir file from disk (seedDir set) or the embedded
// bundle (seedDir empty) and unmarshals it.
func readAsset(seedDir, name string, v any) error {
	if seedDir != "" {
		return readJSONFile(filepath.Join(seedDir, name), v)
	}
	return readJSONEmbedded(name, v)
}

// readOracle reads the oracle from disk (oraclePath set) or the embedded bundle.
func readOracle(oraclePath string, v any) error {
	if oraclePath != "" {
		return readJSONFile(oraclePath, v)
	}
	return readJSONEmbedded("longmemeval_oracle.json", v)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readJSONEmbedded(name string, v any) error {
	b, err := embeddedSeeds.ReadFile(embeddedRoot + "/" + name)
	if err != nil {
		return fmt.Errorf("read embedded %s: %w", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse embedded %s: %w", name, err)
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
