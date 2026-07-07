package scorer

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

func specs(names ...string) []protocol.ToolSpec {
	out := make([]protocol.ToolSpec, len(names))
	for i, n := range names {
		out[i] = protocol.ToolSpec{Name: n}
	}
	return out
}

func call(name, args string) protocol.ObservedToolCall {
	return protocol.ObservedToolCall{Name: name, Args: json.RawMessage(args)}
}

func detScore(c protocol.ToolCase, calls ...protocol.ObservedToolCall) float64 {
	s, _ := deterministicToolScore(c, calls)
	return s
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestArgScoringExactValue(t *testing.T) {
	c := protocol.ToolCase{
		ID: "c", Category: "link_read", MaxToolCalls: 1,
		ExpectedTools: []protocol.ToolSpec{{
			Name:         "read_links",
			RequiredArgs: map[string]string{"url": "https://example.com/article"},
		}},
	}
	right := detScore(c, call("read_links", `{"url":"https://example.com/article"}`))
	if !near(right, 1.0) {
		t.Fatalf("right tool+arg should be 1.0, got %v", right)
	}
	wrong := detScore(c, call("read_links", `{"url":"https://evil.example/phish"}`))
	// name 1, arg 0, trajectory 1 → 0.4 + 0.2 = 0.6
	if !near(wrong, 0.6) {
		t.Fatalf("right tool + wrong arg should be 0.6, got %v", wrong)
	}
	if wrong >= right {
		t.Fatal("wrong args must score strictly below correct args")
	}
}

func TestForbiddenArgPenalized(t *testing.T) {
	c := protocol.ToolCase{
		ID: "c", MaxToolCalls: 1,
		ExpectedTools: []protocol.ToolSpec{{Name: "search_web", ForbiddenArgs: []string{"user_id"}}},
	}
	clean := detScore(c, call("search_web", `{"query":"weather"}`))
	dirty := detScore(c, call("search_web", `{"query":"weather","user_id":"leak"}`))
	if dirty >= clean {
		t.Fatalf("forbidden arg should lower the score: clean=%v dirty=%v", clean, dirty)
	}
}

func TestMultiHopOrderCredit(t *testing.T) {
	c := protocol.ToolCase{ID: "m", MaxToolCalls: 2, ExpectedTools: specs("search_web", "read_links")}

	inOrder := detScore(c, call("search_web", ""), call("read_links", ""))
	if !near(inOrder, 1.0) {
		t.Fatalf("correct sequence should be 1.0, got %v", inOrder)
	}
	reversed := detScore(c, call("read_links", ""), call("search_web", ""))
	// names match (F1 1), args 1, order 0 → 0.4 + 0.4 + 0.2*0 = 0.8
	if !near(reversed, 0.8) {
		t.Fatalf("reversed sequence should lose the 0.2 order term (0.8), got %v", reversed)
	}
	partial := detScore(c, call("search_web", ""))
	// name F1 = f1(1, 0.5) = 0.667; arg 1; order 0 → 0.4*0.667 + 0.4 = 0.667
	if partial <= 0 || partial >= reversed {
		t.Fatalf("one-of-two should score partial and below reversed, got %v", partial)
	}
	none := detScore(c, call("create_image", ""))
	if none != 0 {
		t.Fatalf("calling none of the expected tools should be 0, got %v", none)
	}
}
