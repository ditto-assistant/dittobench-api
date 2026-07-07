package datagen

import (
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/catalog"
)

// TestFullCatalogCoverage checks the property: every tool in the catalog is
// the correct answer (an expected tool) for at least one generated case. A large
// n makes the stratified draw hit every category.
func TestFullCatalogCoverage(t *testing.T) {
	ds := Generate(2026, 200)
	reachable := map[string]bool{}
	for _, c := range ds.ToolCases {
		for _, ts := range c.ExpectedTools {
			reachable[ts.Name] = true
		}
	}
	var missing []string
	for _, tool := range catalog.Catalog() {
		if !reachable[tool.Name] {
			missing = append(missing, tool.Name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("catalog tools never a correct answer: %v", missing)
	}
}
