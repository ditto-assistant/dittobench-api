// tokenbaseline validates trusted starter-kit score reports and emits a
// deterministic, reviewable baseline manifest. It never calls a tokenizer and
// never accepts miner-reported RunResponse token fields: inputs must contain the
// API's model-proxy-derived details.token_usage block.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

const datasetKnownVectorSeed int64 = 123456789

type reportEnvelope struct {
	Report *protocol.ScoreReport `json:"report"`
}

type groupKey struct {
	RunSize         string
	Provider        string
	ProfileRevision string
	Model           string
}

func main() {
	benchVersion := flag.Int("bench-version", protocol.BenchVersionV5, "benchmark contract to calibrate (5 or 7)")
	starterKitRevision := flag.String("starter-kit-revision", "", "immutable starter-kit git revision (required for v7)")
	refreshDatasets := flag.Bool("refresh-datasets", false, "regenerate the pinned calibration dataset hashes and known vector")
	enableScoring := flag.Bool("enable-scoring", false, "emit an enabled candidate after every supplied provider profile validates")
	flag.Parse()
	if *benchVersion != protocol.BenchVersionV5 && *benchVersion != protocol.BenchVersionV7 {
		fatalf("unsupported calibration bench version %d", *benchVersion)
	}
	manifest, err := calibrationManifest(*benchVersion, *starterKitRevision)
	if err != nil {
		fatalf("calibration contract: %v", err)
	}
	if *refreshDatasets {
		if flag.NArg() != 0 {
			fatalf("-refresh-datasets does not accept score reports")
		}
		manifest, err = refreshedDatasetManifestForVersion(manifest)
		if err != nil {
			fatalf("refresh datasets: %v", err)
		}
		printManifest(manifest)
		return
	}
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: tokenbaseline [-refresh-datasets] report.json [report.json ...]")
		os.Exit(2)
	}
	expected := make(map[string]struct{}, len(manifest.Calibration))
	for _, dataset := range manifest.Calibration {
		expected[datasetKey(dataset.RunSize, dataset.Seed, dataset.DatasetSHA256)] = struct{}{}
	}
	groups := map[groupKey][]protocol.ScoreReport{}
	for _, path := range flag.Args() {
		report, err := readReport(path)
		if err != nil {
			fatalf("%s: %v", path, err)
		}
		if report.Details == nil || report.Details.TokenUsage == nil {
			fatalf("%s: missing trusted details.token_usage", path)
		}
		usage := report.Details.TokenUsage
		if report.Details.BenchVersion != *benchVersion ||
			usage.Source != "model_proxy_provider_response" || !efficiency.ValidUsage(*usage) ||
			usage.Provider == "" || usage.ProfileRevision == "" || usage.Model == "" {
			fatalf("%s: report is not a complete v%d proxy-metered run", path, *benchVersion)
		}
		if *benchVersion == protocol.BenchVersionV7 && usage.Model != llm.V7HarnessModel {
			fatalf("%s: report does not match the v7 GPT-OSS contract", path)
		}
		key := datasetKey(report.Details.RunSize, report.Seed, report.Details.DatasetSHA256)
		if _, ok := expected[key]; !ok {
			fatalf("%s: dataset is outside the pinned calibration set", path)
		}
		group := groupKey{report.Details.RunSize, usage.Provider, usage.ProfileRevision, usage.Model}
		groups[group] = append(groups[group], report)
	}

	manifest.Baselines = manifest.Baselines[:0]
	for key, reports := range groups {
		if len(reports) != 20 {
			fatalf("%s/%s/%s: need exactly 20 pinned calibration runs, got %d", key.Provider, key.ProfileRevision, key.RunSize, len(reports))
		}
		seen := map[int64]bool{}
		for _, report := range reports {
			if seen[report.Seed] {
				fatalf("%s/%s/%s: duplicate seed %d", key.Provider, key.ProfileRevision, key.RunSize, report.Seed)
			}
			seen[report.Seed] = true
		}
		sort.Slice(reports, func(i, j int) bool {
			a := reports[i].Details.TokenUsage
			b := reports[j].Details.TokenUsage
			if a.TotalTokens != b.TotalTokens {
				return a.TotalTokens < b.TotalTokens
			}
			if a.PromptTokens != b.PromptTokens {
				return a.PromptTokens < b.PromptTokens
			}
			if a.CompletionTokens != b.CompletionTokens {
				return a.CompletionTokens < b.CompletionTokens
			}
			return reports[i].Seed < reports[j].Seed
		})
		p90Index := int(math.Ceil(efficiency.BudgetPercentile*float64(len(reports)))) - 1
		budget := reports[p90Index].Details.TokenUsage
		baseline := efficiency.Baseline{
			BenchVersion: *benchVersion, RunSize: key.RunSize,
			Provider: key.Provider, ProfileRevision: key.ProfileRevision, Model: key.Model,
			PromptTokens: budget.PromptTokens, CompletionTokens: budget.CompletionTokens,
			TotalTokens: budget.TotalTokens, Samples: len(reports), Aggregation: "nearest_rank_p90",
			StarterKitRevision: manifest.StarterKitRevision,
		}
		if *benchVersion == protocol.BenchVersionV7 {
			baseline.RawReferencePromptTokens = budget.PromptTokens
			baseline.RawReferenceCompletionTokens = budget.CompletionTokens
			baseline.RawReferenceTotalTokens = budget.TotalTokens
			baseline.AllowanceMultiplierBPS = 7500
			baseline.AllowanceTotalTokens = budget.TotalTokens * baseline.AllowanceMultiplierBPS / 10_000
			baseline.AllowancePromptTokens = budget.PromptTokens * baseline.AllowanceMultiplierBPS / 10_000
			baseline.AllowanceCompletionTokens = baseline.AllowanceTotalTokens - baseline.AllowancePromptTokens
			baseline.AllowancePolicy = "starter_raw_p90_floor_75_percent_v1"
			baseline.PromptTokens = 0
			baseline.CompletionTokens = 0
			baseline.TotalTokens = 0
		}
		baseline.ID = baselineID(*benchVersion, baseline)
		manifest.Baselines = append(manifest.Baselines, baseline)
	}
	sortBaselines(manifest.Baselines)
	if *benchVersion == protocol.BenchVersionV7 {
		reviewed := manifest
		reviewed.ScoringEnabled = true
		if !efficiency.ReadyForV7Production(reviewed) {
			fatalf("refusing candidate: raw evidence or derived allowance contract is incomplete")
		}
	}
	if *enableScoring {
		manifest.ScoringEnabled = true
		ready := efficiency.ReadyForProduction(manifest)
		if *benchVersion == protocol.BenchVersionV7 {
			ready = efficiency.ReadyForV7Production(manifest)
		}
		if !ready {
			fatalf("refusing activation: candidate does not contain complete reviewed provider/run-size groups")
		}
	}
	printManifest(manifest)
}

func refreshedDatasetManifest() (efficiency.Manifest, error) {
	manifest, err := calibrationManifest(protocol.BenchVersionV5, "")
	if err != nil {
		return manifest, err
	}
	return refreshedDatasetManifestForVersion(manifest)
}

func calibrationManifest(benchVersion int, starterKitRevision string) (efficiency.Manifest, error) {
	manifest := efficiency.ManifestSnapshot()
	if benchVersion == protocol.BenchVersionV5 {
		return manifest, nil
	}
	if !canonicalGitRevision(starterKitRevision) {
		return manifest, fmt.Errorf("v7 requires a canonical 40-character lowercase starter-kit git revision")
	}
	manifest.BenchVersion = benchVersion
	manifest.StarterKitRevision = starterKitRevision
	manifest.Embedding = &efficiency.EmbeddingContract{
		Provider: "Perplexity", Model: "perplexity/pplx-embed-v1-0.6b",
		Profile: "dittobench-v7-openrouter-pplx-embed-v1-0.6b-768-v1", Dimensions: 768,
		CatalogSHA256: "a6fff568e4f8c9d17e54739e6da2cdd76006d2a486124c5c43b12e94099d3348",
	}
	manifest.DatasetKnownVector = ""
	manifest.ScoringEnabled = false
	manifest.Baselines = nil
	for i := range manifest.Calibration {
		manifest.Calibration[i].DatasetSHA256 = ""
	}
	// V7 is intentionally dark and has no embedded production manifest yet.
	// Derive its pinned dataset hashes from the checked-out datagen revision so
	// completed calibration reports can be audited before the reviewed manifest
	// is embedded. This remains deterministic and fail-closed on any mismatch.
	return refreshedDatasetManifestForVersion(manifest)
}

func canonicalGitRevision(value string) bool {
	if len(value) != 40 || value != strings.ToLower(value) || value == strings.Repeat("0", 40) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sortBaselines(baselines []efficiency.Baseline) {
	sort.Slice(baselines, func(i, j int) bool {
		a, b := baselines[i], baselines[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.ProfileRevision != b.ProfileRevision {
			return a.ProfileRevision < b.ProfileRevision
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.RunSize < b.RunSize
	})
}

func refreshedDatasetManifestForVersion(manifest efficiency.Manifest) (efficiency.Manifest, error) {
	benchVersion := manifest.BenchVersion
	manifest.ScoringEnabled = false
	manifest.Baselines = nil
	knownProfile, ok := gen.ProfileForVersion("full", benchVersion)
	if !ok {
		return manifest, fmt.Errorf("v%d full profile unavailable", benchVersion)
	}
	known, err := gen.GenerateDataset(datasetKnownVectorSeed, knownProfile, benchVersion)
	if err != nil {
		return manifest, fmt.Errorf("generate known vector: %w", err)
	}
	manifest.DatasetKnownVector, _, err = known.SHA256Hex()
	if err != nil {
		return manifest, fmt.Errorf("hash known vector: %w", err)
	}
	for i, dataset := range manifest.Calibration {
		profile, ok := gen.ProfileForVersion(dataset.RunSize, benchVersion)
		if !ok {
			return manifest, fmt.Errorf("unknown run size %q", dataset.RunSize)
		}
		artifact, err := gen.GenerateDataset(dataset.Seed, profile, benchVersion)
		if err != nil {
			return manifest, fmt.Errorf("generate %s seed %d: %w", dataset.RunSize, dataset.Seed, err)
		}
		hash, _, err := artifact.SHA256Hex()
		if err != nil {
			return manifest, fmt.Errorf("hash %s seed %d: %w", dataset.RunSize, dataset.Seed, err)
		}
		manifest.Calibration[i].DatasetSHA256 = hash
	}
	return manifest, nil
}

func printManifest(manifest efficiency.Manifest) {
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fatalf("encode manifest: %v", err)
	}
	fmt.Println(string(out))
}

func readReport(path string) (protocol.ScoreReport, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return protocol.ScoreReport{}, err
	}
	var envelope reportEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return protocol.ScoreReport{}, err
	}
	if envelope.Report != nil {
		return *envelope.Report, nil
	}
	var report protocol.ScoreReport
	if err := json.Unmarshal(body, &report); err != nil {
		return protocol.ScoreReport{}, err
	}
	if report.RunID == "" {
		return protocol.ScoreReport{}, fmt.Errorf("not a score report or run envelope")
	}
	return report, nil
}

func datasetKey(runSize string, seed int64, datasetSHA string) string {
	return fmt.Sprintf("%s:%d:%s", runSize, seed, datasetSHA)
}

func baselineID(benchVersion int, b efficiency.Baseline) string {
	if benchVersion == protocol.BenchVersionV7 {
		canonical := fmt.Sprintf("%s:%s:%s:%s:%s:%d:%d:%d:%d:%d:%d:%d:%s:%s", efficiency.FormulaVersion, b.RunSize, b.Provider,
			b.ProfileRevision, b.Model, b.RawReferencePromptTokens, b.RawReferenceCompletionTokens,
			b.RawReferenceTotalTokens, b.AllowanceMultiplierBPS, b.AllowancePromptTokens,
			b.AllowanceCompletionTokens, b.AllowanceTotalTokens, b.AllowancePolicy, b.StarterKitRevision)
		sum := sha256.Sum256([]byte(canonical))
		return fmt.Sprintf("v%d-starter-p90-%s", benchVersion, hex.EncodeToString(sum[:8]))
	}
	canonical := fmt.Sprintf("%s:%s:%s:%s:%s:%d:%d:%d:%s", efficiency.FormulaVersion, b.RunSize, b.Provider,
		b.ProfileRevision, b.Model, b.PromptTokens, b.CompletionTokens, b.TotalTokens, b.StarterKitRevision)
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("v%d-starter-p90-%s", benchVersion, hex.EncodeToString(sum[:8]))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
