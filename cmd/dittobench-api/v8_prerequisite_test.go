package main

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestToolPrerequisiteWaveCollectsOnlyPrivatePairs(t *testing.T) {
	cases := []protocol.ToolCase{
		{ID: "ordinary"},
		{ID: "routed-a", PrerequisitePairs: []protocol.MemoryPair{{PairID: "p-a"}}},
		{ID: "routed-b", PrerequisitePairs: []protocol.MemoryPair{{PairID: "p-b"}}},
	}
	wave, err := toolPrerequisiteWave(cases)
	if err != nil {
		t.Fatal(err)
	}
	if wave.UserID != "miner" || len(wave.Pairs) != 2 || wave.Pairs[0].PairID != "p-a" || wave.Pairs[1].PairID != "p-b" {
		t.Fatalf("unexpected prerequisite wave: %+v", wave)
	}
}

func TestToolPrerequisiteWaveRejectsDuplicateIdentity(t *testing.T) {
	cases := []protocol.ToolCase{
		{PrerequisitePairs: []protocol.MemoryPair{{PairID: "same"}}},
		{PrerequisitePairs: []protocol.MemoryPair{{PairID: "same"}}},
	}
	if _, err := toolPrerequisiteWave(cases); err == nil {
		t.Fatal("duplicate prerequisite pair accepted")
	}
}
