package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ditto-assistant/dittobench-api/internal/netguard"
)

// Tarball ingestion (SN118 "mode B"): the platform stores a miner's harness as a
// gzipped tar in object storage and hands the validator a short-lived presigned
// URL. Unlike a git clone, this blob is fully attacker-controlled and is
// unpacked on the validator's build host (outside the Docker sandbox), so the
// fetch + extract here is the trust boundary and is hardened accordingly:
//
//   - fetch is SSRF-guarded (netguard, re-checked at dial vs DNS rebinding);
//   - the compressed download is size-capped;
//   - the uncompressed output is size- and file-count-capped (gzip-bomb / inode
//     exhaustion);
//   - extraction rejects absolute paths and "../" escapes ("zip slip") and skips
//     symlinks/hardlinks/devices (the classic untrusted-tar host escape).
//
// Limits are package vars (not consts) so tests can shrink them to exercise the
// guards without pushing tens of MiB through the extractor. Production never
// mutates them.
var (
	// maxTarballBytes caps the compressed download. The platform caps uploads at
	// 2 MiB (MAX_TARBALL_SIZE_BYTES); 8 MiB leaves headroom while still bounding
	// a hostile presigned URL that streams an unbounded body.
	maxTarballBytes int64 = 8 << 20 // 8 MiB compressed
	// maxExtractedBytes caps total uncompressed output (gzip-bomb guard: a few
	// MiB of gzip can inflate to gigabytes).
	maxExtractedBytes int64 = 64 << 20 // 64 MiB uncompressed
	// maxTarballFiles caps the member count (many-tiny-files / inode DoS).
	maxTarballFiles = 20000
)

// fetchTarball downloads src.TarballURL, extracts it under workdir, and returns
// the directory to use as the Docker build context (the extraction root, or a
// lone top-level directory that holds the Dockerfile — miners may pack either
// `./Dockerfile` or `harness/Dockerfile`). See the file header for the threat
// model. On any error the caller discards workdir, so partial extractions never
// reach a build.
func (d *LocalDocker) fetchTarball(ctx context.Context, src Source, workdir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.TarballURL, nil)
	if err != nil {
		return "", fmt.Errorf("tarball request: %w", err)
	}
	resp, err := netguard.Client(d.AllowPrivate).Do(req)
	if err != nil {
		return "", fmt.Errorf("tarball fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tarball fetch: unexpected status %d", resp.StatusCode)
	}

	// Cap the compressed stream (+1 byte so an over-cap body is detectable), and
	// hash it in passing to verify the pinned SHA-256 without a second read.
	limited := &io.LimitedReader{R: resp.Body, N: maxTarballBytes + 1}
	hasher := sha256.New()
	tee := io.TeeReader(limited, hasher)

	if err := extractTarGz(tee, workdir); err != nil {
		if limited.N <= 0 {
			return "", fmt.Errorf("tarball exceeds %d bytes", maxTarballBytes)
		}
		return "", err
	}
	// tar stops at its end-of-archive marker, which may precede the gzip trailer;
	// drain the rest so the hash and the size cap cover the full body.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return "", fmt.Errorf("tarball read: %w", err)
	}
	if limited.N <= 0 {
		return "", fmt.Errorf("tarball exceeds %d bytes", maxTarballBytes)
	}
	if src.TarballSHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, strings.TrimSpace(src.TarballSHA256)) {
			return "", fmt.Errorf("tarball sha256 mismatch: got %s", got)
		}
	}
	return buildContextDir(workdir)
}

// extractTarGz streams a gzipped tar from r into dst, enforcing the size, count,
// and path-safety limits described in the file header.
func extractTarGz(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var total int64
	var files int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if files++; files > maxTarballFiles {
			return fmt.Errorf("tarball has too many entries (> %d)", maxTarballFiles)
		}

		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", hdr.Name, err)
			}
			budget := maxExtractedBytes - total
			if budget <= 0 {
				return fmt.Errorf("extracted size exceeds %d bytes", maxExtractedBytes)
			}
			n, err := writeRegular(target, tr, hdr.FileInfo().Mode().Perm(), budget)
			total += n
			if err != nil {
				return err
			}
		default:
			// Skip symlinks, hardlinks, fifos, devices, char/block specials — none
			// belong in a build context and a symlink escape is a host-write vector.
			continue
		}
	}
	return nil
}

// safeJoin resolves name under root, rejecting absolute paths and any "../"
// escape so a malicious member cannot be written outside the extraction root.
func safeJoin(root, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("tar entry has an empty name")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return "", fmt.Errorf("tar entry %q is an absolute path", name)
	}
	target := filepath.Join(root, filepath.Clean(name))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", fmt.Errorf("tar entry %q: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar entry %q escapes the extraction root", name)
	}
	return target, nil
}

// writeRegular writes up to budget bytes from r into path, returning the bytes
// written. Exceeding budget is an error (per-file half of the gzip-bomb guard).
func writeRegular(path string, r io.Reader, perm os.FileMode, budget int64) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, sanePerm(perm))
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	// budget+1 so a file exactly at the remaining budget still fits; > trips.
	n, err := io.Copy(f, io.LimitReader(r, budget+1))
	if err != nil {
		return n, fmt.Errorf("write %s: %w", path, err)
	}
	if n > budget {
		return n, fmt.Errorf("extracted size exceeds %d bytes", maxExtractedBytes)
	}
	return n, nil
}

// sanePerm drops setuid/setgid/sticky and guarantees owner read+write, keeping
// any execute bit (build scripts) while never honoring odd modes from the tar.
func sanePerm(m os.FileMode) os.FileMode {
	p := m.Perm() & 0o777
	return p | 0o600
}

// buildContextDir picks the Docker build context for an extracted tarball: the
// root if it holds a Dockerfile, else a single top-level directory that does.
// Ambiguous layouts (no Dockerfile, or multiple top-level dirs) are an error —
// we won't guess.
func buildContextDir(root string) (string, error) {
	if isFile(filepath.Join(root, "Dockerfile")) {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read extraction root: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 {
		sub := filepath.Join(root, dirs[0])
		if isFile(filepath.Join(sub, "Dockerfile")) {
			return sub, nil
		}
	}
	return "", fmt.Errorf("no Dockerfile at the tarball root or a single top-level directory")
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
