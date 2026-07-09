package toolexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

func webCase(id string) protocol.ToolCase {
	return protocol.ToolCase{
		ID:            id,
		Category:      "web_search",
		ExpectedTools: []protocol.ToolSpec{{Name: "search_web"}},
		MaxToolCalls:  1,
	}
}

func TestServesAndObservable(t *testing.T) {
	if Serves("search_memories") || Serves("fetch_memories") {
		t.Fatal("memory tools must not be served")
	}
	if !Serves("search_web") || !Serves("get_agent_job_status") || !Serves("set_theme") {
		t.Fatal("external-world tools must be served")
	}
	if Serves("") {
		t.Fatal("empty tool name is not served")
	}

	if !Observable(webCase("w")) {
		t.Fatal("all-served case should be observable")
	}
	memCase := protocol.ToolCase{ExpectedTools: []protocol.ToolSpec{{Name: "search_memories"}}}
	if Observable(memCase) {
		t.Fatal("memory case is not observable")
	}
	mixed := protocol.ToolCase{ExpectedTools: []protocol.ToolSpec{{Name: "search_web"}, {Name: "search_memories_in_subjects"}}}
	if Observable(mixed) {
		t.Fatal("mixed case with a memory tool is not observable")
	}
	if Observable(protocol.ToolCase{}) {
		t.Fatal("no-expected-tool case is not observable")
	}
}

func TestBuildFixtureDeterministic(t *testing.T) {
	c := webCase("web_search-42-0001")
	a := BuildFixture(42, c)
	b := BuildFixture(42, c)
	if a.NeedleText() != b.NeedleText() {
		t.Fatalf("same seed must give same needle: %q vs %q", a.NeedleText(), b.NeedleText())
	}
	if a.NeedleText() == "" || a.NeedleValue() == "" || a.Subject() == "" {
		t.Fatal("content case should carry a needle (subject + value + text)")
	}
	// Different seed → (almost surely) different needle.
	if BuildFixture(43, c).NeedleText() == a.NeedleText() {
		t.Fatal("different seed should change the needle")
	}
	// NeedleFor is the shared deriver both the fixture and prompt use — coherent.
	if NeedleFor(42, c.ID).Value != a.NeedleValue() {
		t.Fatal("NeedleFor must match the fixture's served needle")
	}
	// A settings case carries no needle (confirmation-only tool).
	settings := protocol.ToolCase{ID: "settings-1-0", ExpectedTools: []protocol.ToolSpec{{Name: "set_theme"}}}
	if BuildFixture(1, settings).NeedleText() != "" {
		t.Fatal("settings case should carry no needle")
	}
}

func TestResultDeterministicAndTyped(t *testing.T) {
	f := BuildFixture(7, webCase("web_search-7-0"))
	r1, ok := f.Result("search_web", nil)
	if !ok || r1 == "" {
		t.Fatal("search_web should return content")
	}
	if r2, _ := f.Result("search_web", nil); r2 != r1 {
		t.Fatal("same call must be deterministic")
	}
	// The needle value (the distinctive number) is present in a content result.
	nv := f.NeedleValue()
	if nv == "" || !strings.Contains(r1, nv) {
		t.Fatalf("needle value %q should appear in web result %q", nv, r1)
	}
	// A non-served (memory) tool is not answered.
	if _, ok := f.Result("search_memories", nil); ok {
		t.Fatal("memory tool must not be served")
	}
}

func TestResultDistinctByArgs(t *testing.T) {
	f := BuildFixture(9, protocol.ToolCase{ID: "multi_web_read-9-0",
		ExpectedTools: []protocol.ToolSpec{{Name: "read_links"}}})
	a, _ := f.Result("read_links", json.RawMessage(`{"url":"https://a.example"}`))
	b, _ := f.Result("read_links", json.RawMessage(`{"url":"https://b.example"}`))
	if a == b {
		t.Fatal("read_links on different URLs should vary")
	}
	// ...but stable per arg set (and order-independent).
	b2, _ := f.Result("read_links", json.RawMessage(`{"url":"https://b.example"}`))
	if b != b2 {
		t.Fatal("same args must be stable")
	}
}

func TestServerObservesAndServes(t *testing.T) {
	s := NewServer()
	c := webCase("web_search-1-0")
	s.Register(c.ID, BuildFixture(1, c))
	ts := httptest.NewServer(s)
	defer ts.Close()

	call := func(name string, hop int, args string) protocol.ToolExecResponse {
		body, _ := json.Marshal(protocol.ToolExecRequest{
			CaseID: c.ID, Name: name, Hop: hop, Args: json.RawMessage(args),
		})
		resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		var out protocol.ToolExecResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if r := call("search_web", 0, `{"query":"x"}`); r.Result == "" || r.Error != "" {
		t.Fatalf("served tool should return a result, got %+v", r)
	}
	// A memory tool routed here is recorded but errored.
	if r := call("search_memories", 1, `{"query":"y"}`); r.Error == "" {
		t.Fatalf("memory tool should error, got %+v", r)
	}

	obs := s.Observed(c.ID)
	if len(obs) != 2 {
		t.Fatalf("expected 2 observed calls, got %d", len(obs))
	}
	if obs[0].Name != "search_web" || obs[1].Name != "search_memories" {
		t.Fatalf("observed order wrong: %+v", obs)
	}
	if s.Observed("no-such-case") != nil {
		t.Fatal("unknown case should observe nothing")
	}
}

func TestServerUnknownCase(t *testing.T) {
	s := NewServer()
	ts := httptest.NewServer(s)
	defer ts.Close()
	body, _ := json.Marshal(protocol.ToolExecRequest{CaseID: "ghost", Name: "search_web"})
	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown case should 404, got %d", resp.StatusCode)
	}
}
