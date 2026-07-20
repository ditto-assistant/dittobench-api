// tokenbaseline validates trusted v5 starter-kit score reports and emits a
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
	"os"
	"sort"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

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
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: tokenbaseline report.json [report.json ...]")
		os.Exit(2)
	}
	manifest := efficiency.ManifestSnapshot()
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
		if report.Details.BenchVersion != protocol.BenchVersionV5 ||
			usage.Source != "model_proxy_provider_response" || !efficiency.ValidUsage(*usage) ||
			usage.Provider == "" || usage.ProfileRevision == "" || usage.Model == "" {
			fatalf("%s: report is not a complete v5 proxy-metered run", path)
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
		if len(reports) != 5 {
			fatalf("%s/%s/%s: need exactly 5 pinned calibration runs, got %d", key.Provider, key.ProfileRevision, key.RunSize, len(reports))
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
			aw := efficiency.WeightedTokens(a.PromptTokens, a.CompletionTokens)
			bw := efficiency.WeightedTokens(b.PromptTokens, b.CompletionTokens)
			if aw != bw {
				return aw < bw
			}
			if a.PromptTokens != b.PromptTokens {
				return a.PromptTokens < b.PromptTokens
			}
			if a.CompletionTokens != b.CompletionTokens {
				return a.CompletionTokens < b.CompletionTokens
			}
			return reports[i].Seed < reports[j].Seed
		})
		median := reports[len(reports)/2].Details.TokenUsage
		baseline := efficiency.Baseline{
			BenchVersion: protocol.BenchVersionV5, RunSize: key.RunSize,
			Provider: key.Provider, ProfileRevision: key.ProfileRevision, Model: key.Model,
			PromptTokens: median.PromptTokens, CompletionTokens: median.CompletionTokens,
			TotalTokens: median.TotalTokens, Samples: len(reports), Aggregation: "median",
			StarterKitRevision: manifest.StarterKitRevision,
		}
		baseline.ID = baselineID(baseline)
		manifest.Baselines = append(manifest.Baselines, baseline)
	}
	sort.Slice(manifest.Baselines, func(i, j int) bool {
		a, b := manifest.Baselines[i], manifest.Baselines[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.RunSize < b.RunSize
	})
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

func baselineID(b efficiency.Baseline) string {
	canonical := fmt.Sprintf("%s:%s:%s:%s:%s:%d:%d:%d:%s", efficiency.FormulaVersion, b.RunSize, b.Provider,
		b.ProfileRevision, b.Model, b.PromptTokens, b.CompletionTokens, b.TotalTokens, b.StarterKitRevision)
	sum := sha256.Sum256([]byte(canonical))
	return "v5-starter-median-" + hex.EncodeToString(sum[:8])
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
