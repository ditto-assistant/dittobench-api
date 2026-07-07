package main

import "testing"

func TestSameModelFamily(t *testing.T) {
	same := [][2]string{
		{"openai/gpt-4o", "gpt-4o"},
		{"gpt-4o", "gpt-4o-2024-11-20"},
		{"anthropic/claude-sonnet-5", "claude-sonnet-5"},
		{"google/gemini-3-pro", "gemini-3-pro"},
	}
	for _, p := range same {
		if !sameModelFamily(p[0], p[1]) {
			t.Errorf("expected %q and %q to match", p[0], p[1])
		}
	}
	diff := [][2]string{
		{"openai/gpt-4o", "claude-sonnet-5"},
		{"gemini-3-pro", "gemini-3-flash"},
		{"", "gpt-4o"},
	}
	for _, p := range diff {
		if sameModelFamily(p[0], p[1]) {
			t.Errorf("expected %q and %q to differ", p[0], p[1])
		}
	}
}
