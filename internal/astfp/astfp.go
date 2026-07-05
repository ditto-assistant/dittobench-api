// Package astfp computes an AST-level structural fingerprint of a submitted
// Rust harness crate. It is the structural counterpart to the platform's lexical
// content fingerprint (ditto-platform ditto/api_server/fingerprint.py): where the
// lexical signal survives reformatting and localized edits, this survives
// identifier renaming and reformatting because it hashes only the *shape* of the
// parse tree, never the text of identifiers or literals.
//
// How it works. Each .rs file under the extracted crate is parsed with
// tree-sitter-rust; a pre-order walk emits the sequence of *named node types*
// (e.g. function_item, let_declaration, binary_expression) — the leaves that
// carry a name or a literal contribute only their node type, never their text,
// so renaming every variable leaves the stream identical. The per-file streams
// are cut into overlapping k-node shingles, hashed, and unioned across files, so
// renaming/reordering files is invisible and a localized edit disturbs only the
// few shingles spanning it.
//
// Storage is a bottom-k MinHash (KMV) sketch in exactly the wire shape the
// platform already compares (`{v,k,card,m}`), so the platform reuses its own
// similarity estimator on the structural channel. This fingerprint travels in the
// ScoreReport as advisory (unsigned) moderation metadata; the platform's anti-copy
// gate holds a cross-miner structural near-duplicate for human review.
//
// It is computed here, in the scorer, because this is where the crate is already
// unpacked and a Rust parser is available — the platform (a Python payment
// service) has neither. Everything is pure + deterministic: the same crate always
// yields the same sketch.
package astfp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/rust"
)

const (
	// Version stamps the sketch format. It must match the platform sketch format
	// version so the two sides compare (see the platform's content_similarity,
	// which requires equal versions). Bump on any change to normalization,
	// shingling, or the sketch shape.
	Version = 1
	// A shingle is this many consecutive named node types. Small enough that a
	// localized edit disturbs few shingles; large enough to be distinctive.
	shingleNodes = 6
	// Bottom-k sketch budget — fixed-size summary, exact when the shingle set is
	// smaller than k (the common case).
	minhashK = 256
	// Width of each shingle hash as zero-padded hex (64 bits): fixed width so the
	// hex strings sort in the same order as the integers they encode.
	hashHex = 16

	// Work/DoS guards, mirroring the sandbox extractor's posture. The crate is
	// already extracted from a size-capped tarball, but a hostile tree can still
	// be pathological, so the walk is bounded independently.
	maxFiles     = 5000
	maxFileBytes = 8 * 1024 * 1024
	maxShingles  = 500_000
)

// FromDir walks the extracted crate at dir and returns its structural
// fingerprint (a protocol.CodeFingerprint sketch), or nil when the crate has no
// parseable Rust or trips a guard.
//
// It never returns an error: fingerprinting is a best-effort moderation signal
// layered on an already-verified, about-to-be-scored submission, so a crate that
// defeats it simply yields no structural signal (the platform then relies on its
// lexical + size signals). A cancelled ctx aborts the walk and yields nil.
func FromDir(ctx context.Context, dir string) *protocol.CodeFingerprint {
	parser := sitter.NewParser()
	parser.SetLanguage(rust.GetLanguage())

	shingles := map[string]struct{}{}
	files := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than abort the whole walk
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.HasSuffix(path, ".rs") {
			return nil
		}
		files++
		if files > maxFiles {
			return fs.SkipAll
		}
		src, rerr := readCapped(path, maxFileBytes)
		if rerr != nil {
			return nil
		}
		for _, s := range fileShingles(ctx, parser, src) {
			shingles[s] = struct{}{}
			if len(shingles) > maxShingles {
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil || len(shingles) == 0 {
		return nil
	}
	return sketch(shingles)
}

// readCapped reads at most max bytes of a file; a file larger than the cap is
// skipped entirely (returns an error) so a truncated read can't produce a hash a
// smaller honest file could collide with.
func readCapped(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, max+1)
	n, _ := f.Read(buf)
	if n > max {
		return nil, fs.ErrInvalid
	}
	return buf[:n], nil
}

// fileShingles parses one file and returns the hashed k-node shingles of its
// named-node-type stream. Fewer than shingleNodes named nodes yields one shingle
// of the whole stream (nil for an empty/ unparseable file).
func fileShingles(ctx context.Context, parser *sitter.Parser, src []byte) []string {
	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()
	stream := namedTypeStream(tree.RootNode())
	if len(stream) == 0 {
		return nil
	}
	if len(stream) <= shingleNodes {
		return []string{hashShingle(strings.Join(stream, " "))}
	}
	out := make([]string, 0, len(stream)-shingleNodes+1)
	for i := 0; i+shingleNodes <= len(stream); i++ {
		out = append(out, hashShingle(strings.Join(stream[i:i+shingleNodes], " ")))
	}
	return out
}

// namedTypeStream is the pre-order sequence of named node *types* under n.
// Only the type is taken, never the source text, so identifier/literal renaming
// is invisible; anonymous nodes (punctuation, keywords) are skipped so trivial
// token spelling does not enter the fingerprint.
func namedTypeStream(n *sitter.Node) []string {
	var out []string
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if node.IsNamed() {
			out = append(out, node.Type())
		}
		for i := 0; i < int(node.NamedChildCount()); i++ {
			walk(node.NamedChild(i))
		}
	}
	walk(n)
	return out
}

func hashShingle(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashHex]
}

// sketch builds the bottom-k MinHash from a shingle-hash set.
func sketch(shingles map[string]struct{}) *protocol.CodeFingerprint {
	all := make([]string, 0, len(shingles))
	for s := range shingles {
		all = append(all, s)
	}
	sort.Strings(all)
	m := all
	if len(m) > minhashK {
		m = m[:minhashK]
	}
	return &protocol.CodeFingerprint{V: Version, K: minhashK, Card: len(shingles), M: m}
}
