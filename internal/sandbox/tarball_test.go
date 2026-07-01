package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tentry is a minimal tar member for building fixtures.
type tentry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func makeTarGz(t *testing.T, entries []tentry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tf, Linkname: e.linkname}
		if tf == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serve returns a test server that responds to any GET with body.
func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// AllowPrivate is required because the test server binds 127.0.0.1, which the
// SSRF guard would otherwise refuse.
func localDocker() *LocalDocker { return &LocalDocker{AllowPrivate: true} }

func TestFetchTarball_RootDockerfile(t *testing.T) {
	data := makeTarGz(t, []tentry{
		{name: "Dockerfile", body: "FROM scratch\n"},
		{name: "src/main.rs", body: "fn main() {}"},
	})
	srv := serve(t, data)
	work := t.TempDir()

	ctxDir, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, work)
	if err != nil {
		t.Fatalf("fetchTarball: %v", err)
	}
	if ctxDir != work {
		t.Fatalf("context dir = %q, want extraction root %q", ctxDir, work)
	}
	if !isFile(filepath.Join(work, "Dockerfile")) {
		t.Fatal("Dockerfile not extracted at root")
	}
	if !isFile(filepath.Join(work, "src", "main.rs")) {
		t.Fatal("nested file not extracted")
	}
}

func TestFetchTarball_SingleSubdirDockerfile(t *testing.T) {
	data := makeTarGz(t, []tentry{
		{name: "harness/", typeflag: tar.TypeDir},
		{name: "harness/Dockerfile", body: "FROM scratch\n"},
		{name: "harness/Cargo.toml", body: "[package]"},
	})
	srv := serve(t, data)
	work := t.TempDir()

	ctxDir, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, work)
	if err != nil {
		t.Fatalf("fetchTarball: %v", err)
	}
	want := filepath.Join(work, "harness")
	if ctxDir != want {
		t.Fatalf("context dir = %q, want single subdir %q", ctxDir, want)
	}
}

func TestFetchTarball_NoDockerfile(t *testing.T) {
	data := makeTarGz(t, []tentry{{name: "README.md", body: "hi"}})
	srv := serve(t, data)
	if _, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, t.TempDir()); err == nil {
		t.Fatal("expected error when no Dockerfile is present")
	}
}

// TestFetchTarball_ZipSlip is the key host-safety test: a member named "../…"
// must be rejected and must NOT write outside the extraction root.
func TestFetchTarball_ZipSlip(t *testing.T) {
	data := makeTarGz(t, []tentry{
		{name: "Dockerfile", body: "FROM scratch\n"},
		{name: "../pwned.txt", body: "escaped"},
	})
	srv := serve(t, data)
	parent := t.TempDir()
	work := filepath.Join(parent, "extract")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, work); err == nil {
		t.Fatal("expected zip-slip to be rejected")
	}
	if _, err := os.Stat(filepath.Join(parent, "pwned.txt")); err == nil {
		t.Fatal("zip-slip escaped the extraction root")
	}
}

func TestFetchTarball_AbsolutePathRejected(t *testing.T) {
	data := makeTarGz(t, []tentry{{name: "/etc/pwned", body: "x"}})
	srv := serve(t, data)
	if _, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, t.TempDir()); err == nil {
		t.Fatal("expected absolute-path member to be rejected")
	}
}

// TestFetchTarball_SymlinkSkipped: a symlink member (the classic escape vector)
// is silently skipped, not materialized.
func TestFetchTarball_SymlinkSkipped(t *testing.T) {
	data := makeTarGz(t, []tentry{
		{name: "Dockerfile", body: "FROM scratch\n"},
		{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	srv := serve(t, data)
	work := t.TempDir()

	if _, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, work); err != nil {
		t.Fatalf("fetchTarball: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(work, "evil")); err == nil {
		t.Fatal("symlink member should not have been created")
	}
}

func TestFetchTarball_SHA256(t *testing.T) {
	data := makeTarGz(t, []tentry{{name: "Dockerfile", body: "FROM scratch\n"}})
	srv := serve(t, data)
	sum := sha256.Sum256(data)
	good := hex.EncodeToString(sum[:])

	// Matching digest (upper-cased to prove case-insensitive compare) → ok.
	if _, err := localDocker().fetchTarball(context.Background(),
		Source{TarballURL: srv.URL, TarballSHA256: strings.ToUpper(good)}, t.TempDir()); err != nil {
		t.Fatalf("matching sha256 rejected: %v", err)
	}
	// Wrong digest → error.
	if _, err := localDocker().fetchTarball(context.Background(),
		Source{TarballURL: srv.URL, TarballSHA256: "00" + good[2:]}, t.TempDir()); err == nil {
		t.Fatal("expected sha256 mismatch to be rejected")
	}
}

func TestFetchTarball_CompressedSizeCap(t *testing.T) {
	// A tarball whose compressed size exceeds the (shrunk) cap is rejected.
	orig := maxTarballBytes
	maxTarballBytes = 256
	defer func() { maxTarballBytes = orig }()

	// Incompressible-ish payload so the gzip output clears 256 bytes.
	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte(i * 7)
	}
	data := makeTarGz(t, []tentry{
		{name: "Dockerfile", body: "FROM scratch\n"},
		{name: "blob.bin", body: string(big)},
	})
	srv := serve(t, data)
	if _, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, t.TempDir()); err == nil {
		t.Fatal("expected oversized compressed tarball to be rejected")
	}
}

func TestFetchTarball_ExtractedSizeCap(t *testing.T) {
	// Uncompressed output over the (shrunk) cap is rejected — the gzip-bomb guard.
	orig := maxExtractedBytes
	maxExtractedBytes = 512
	defer func() { maxExtractedBytes = orig }()

	data := makeTarGz(t, []tentry{
		{name: "Dockerfile", body: "FROM scratch\n"},
		{name: "big.txt", body: strings.Repeat("A", 4096)},
	})
	srv := serve(t, data)
	if _, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, t.TempDir()); err == nil {
		t.Fatal("expected oversized extracted output to be rejected")
	}
}

func TestFetchTarball_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // e.g. an expired presigned URL
	}))
	t.Cleanup(srv.Close)
	if _, err := localDocker().fetchTarball(context.Background(), Source{TarballURL: srv.URL}, t.TempDir()); err == nil {
		t.Fatal("expected non-200 fetch to error")
	}
}

func TestSafeJoin(t *testing.T) {
	root := "/tmp/root"
	ok := []string{"Dockerfile", "src/main.rs", "./a/b", "a/../b"}
	for _, n := range ok {
		if _, err := safeJoin(root, n); err != nil {
			t.Errorf("safeJoin(%q) unexpected error: %v", n, err)
		}
	}
	bad := []string{"", "../escape", "a/../../escape", "/abs/path", `\abs`}
	for _, n := range bad {
		if _, err := safeJoin(root, n); err == nil {
			t.Errorf("safeJoin(%q) should have been rejected", n)
		}
	}
}

func TestBuildContextDir(t *testing.T) {
	// Root Dockerfile.
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, "Dockerfile"), "FROM scratch")
	if got, err := buildContextDir(root); err != nil || got != root {
		t.Fatalf("root case: got %q err %v", got, err)
	}

	// Single subdir with Dockerfile.
	base := t.TempDir()
	sub := filepath.Join(base, "harness")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(sub, "Dockerfile"), "FROM scratch")
	if got, err := buildContextDir(base); err != nil || got != sub {
		t.Fatalf("subdir case: got %q err %v", got, err)
	}

	// No Dockerfile anywhere → error.
	empty := t.TempDir()
	writeFileT(t, filepath.Join(empty, "README"), "hi")
	if _, err := buildContextDir(empty); err == nil {
		t.Fatal("expected error when no Dockerfile is present")
	}
}

func TestSafeTagTarball(t *testing.T) {
	// Presigned URL (query stripped, suffix stripped, sha suffixed).
	got := safeTag(Source{
		TarballURL:    "https://storage.googleapis.com/bucket/abc123.tar.gz?X-Goog-Signature=deadbeef",
		TarballSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if strings.ContainsAny(got, "?/") || strings.Contains(got, ".tar.gz") {
		t.Fatalf("tag %q leaks url/query/suffix", got)
	}
	if !strings.Contains(got, "abc123") || !strings.Contains(got, "0123456789ab") {
		t.Fatalf("tag %q missing name or sha prefix", got)
	}
}

func writeFileT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
