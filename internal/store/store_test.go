package store

import (
	"sync"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

func TestPutGet(t *testing.T) {
	s := New()
	if _, ok := s.Get("nope"); ok {
		t.Fatalf("expected miss for unknown id")
	}
	s.Put(protocol.ScoreReport{RunID: "r1", Composite: 0.5})
	got, ok := s.Get("r1")
	if !ok || got.Composite != 0.5 {
		t.Fatalf("expected hit with composite 0.5, got ok=%v %+v", ok, got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "r"
			s.Put(protocol.ScoreReport{RunID: id, N: n})
			s.Get(id)
		}(i)
	}
	wg.Wait()
}
