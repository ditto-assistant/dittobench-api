package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestLocalToolServerCanAdvertiseContainerAlias(t *testing.T) {
	t.Setenv("DITTOBENCH_LOCAL_TOOL_HOST", "validator")
	s := &server{allowPrivate: true}
	endpoint, stop, err := s.startToolServer(http.NotFoundHandler(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !strings.HasPrefix(endpoint, "http://validator:") || !strings.HasSuffix(endpoint, "/tool") {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestLocalToolServerAliasRequiresPrivateHarnessMode(t *testing.T) {
	t.Setenv("DITTOBENCH_LOCAL_TOOL_HOST", "validator")
	s := &server{}
	endpoint, stop, err := s.startToolServer(http.NotFoundHandler(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if endpoint != "" {
		t.Fatalf("endpoint = %q, want empty", endpoint)
	}
}
