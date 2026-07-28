package sandbox

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ditto-assistant/dittobench-api/internal/netguard"
)

var maxScreenedImageBytes int64 = 8 << 30 // 8 GiB, additionally exact-size pinned
var screenedImageLoadMu sync.Mutex

const (
	ociImageIndexMediaType    = "application/vnd.oci.image.index.v1+json"
	ociImageManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	ociImageConfigMediaType   = "application/vnd.oci.image.config.v1+json"
	inTotoMediaType           = "application/vnd.in-toto+json"
	maxAttestationDescriptors = 32
	maxAttachmentBlobBytes    = 16 << 20
	maxAttachmentTotalBytes   = 64 << 20
)

// ErrScreenedImageUnavailable marks a failure to ACQUIRE the platform-produced
// screened image, as opposed to a failure to VERIFY it. The distinction is the
// entire safety argument for treating one as the validator's fault and the
// other as terminal, so it is a typed sentinel rather than a substring match on
// an error message that can drift.
//
// Wrapped ONLY by conditions that are transient by construction: a network
// transport error, a 5xx/429 from the object store, a local filesystem or
// Docker daemon fault. Each is a property of THIS validator host or THIS
// network path at THIS moment, and a later attempt on another validator can
// legitimately succeed.
//
// Deliberately NOT wrapped by any verification failure -- sha256 / size / image
// id mismatch, a malformed URL, a 4xx other than 429, or archive and
// attestation rejection. Those are deterministic in the bytes the platform
// stored: they will fail identically on every future attempt, so marking them
// no-fault would re-lease the same broken artifact forever. That is exactly the
// loop that let the mnemox family reach 10 attempts against a budget of 2 with
// zero scores while holding validator slots.
var ErrScreenedImageUnavailable = errors.New("screened image unavailable")

// unavailable wraps err as a transient acquisition failure.
func unavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrScreenedImageUnavailable, err)
}

// retriableFetchStatus reports whether an object-store response status could
// plausibly differ on a later attempt. 5xx is server-side by definition and 429
// is explicitly "come back later". Every other non-200 -- 403 on an expired or
// unauthorized grant, 404 on a missing blob -- is treated as terminal: a run
// must not become no-fault because the platform lost or never wrote the object,
// or the ticket would be re-leased indefinitely against a blob that is not
// coming back.
func retriableFetchStatus(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
	Platform    *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

func (d *LocalDocker) loadScreenedImage(ctx context.Context, src Source, workdir string) (string, string, error) {
	if src.ScreenedImageSize <= 0 || src.ScreenedImageSize > maxScreenedImageBytes {
		return "", "", fmt.Errorf("screened image size %d is outside the allowed range", src.ScreenedImageSize)
	}
	path := filepath.Join(workdir, "screened-image.tar")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, src.ScreenedImageURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("screened image request: %s", redactURL(err.Error(), src.ScreenedImageURL))
	}
	response, err := netguard.Client(d.AllowPrivate).Do(request)
	if err != nil {
		// Transport: DNS, TLS, connection refused, timeout. Nothing about the
		// artifact; the miner's code has not run and cannot run.
		return "", "", unavailable(fmt.Errorf("screened image fetch: %s", redactURL(err.Error(), src.ScreenedImageURL)))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("screened image fetch: unexpected status %d", response.StatusCode)
		if retriableFetchStatus(response.StatusCode) {
			return "", "", unavailable(err)
		}
		return "", "", err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// Local scratch disk: full, read-only, or out of inodes. Host condition.
		return "", "", unavailable(fmt.Errorf("create screened image: %w", err))
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(response.Body, src.ScreenedImageSize+1))
	closeErr := file.Close()
	if copyErr != nil {
		// Stream truncated mid-download (reset, timeout, disk full).
		return "", "", unavailable(fmt.Errorf("screened image download: %w", copyErr))
	}
	if closeErr != nil {
		return "", "", unavailable(fmt.Errorf("close screened image: %w", closeErr))
	}
	if written != src.ScreenedImageSize {
		return "", "", fmt.Errorf("screened image size mismatch: expected %d got %d", src.ScreenedImageSize, written)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, src.ScreenedImageSHA256) {
		return "", "", fmt.Errorf("screened image sha256 mismatch: got %s", got)
	}
	archiveTagged, err := validateDockerSaveArchive(path, src.ScreenedImageRef, src.ScreenedImageID)
	if err != nil {
		return "", "", fmt.Errorf("invalid screened image archive: %w", err)
	}
	screenedImageLoadMu.Lock()
	defer screenedImageLoadMu.Unlock()
	// Preserve an operator/pre-existing tag if this UUID-like screener ref was
	// somehow already present. Every exit after docker load restores the prior
	// tag (or removes the newly imported one), including partial load failures.
	priorID := d.inspectImageID(ctx, src.ScreenedImageRef)
	loadOut, err := exec.CommandContext(ctx, "docker", "image", "load", "--input", path).CombinedOutput()
	if err != nil {
		_ = d.restoreImageRef(ctx, src.ScreenedImageRef, priorID)
		return "", tail(string(loadOut), 4000), fmt.Errorf("docker image load failed: %w", err)
	}
	cleanupSourceRef := true
	defer func() {
		if cleanupSourceRef {
			_ = d.restoreImageRef(context.Background(), src.ScreenedImageRef, priorID)
		}
	}()
	if !archiveTagged {
		// `docker image save <immutable-id>` intentionally carries no RepoTags.
		// The archive parser proved there is exactly one image and bound its bytes
		// to the signed ID; materialize the expected ref from that immutable ID.
		loadedID := d.inspectImageID(ctx, src.ScreenedImageID)
		if loadedID != src.ScreenedImageID {
			return "", tail(string(loadOut), 4000), fmt.Errorf("untagged screened image id mismatch: expected %s got %s", src.ScreenedImageID, loadedID)
		}
		if out, err := exec.CommandContext(ctx, "docker", "image", "tag", src.ScreenedImageID, src.ScreenedImageRef).CombinedOutput(); err != nil {
			return "", tail(string(loadOut), 4000), fmt.Errorf("tag untagged screened image: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	inspectOut, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", src.ScreenedImageRef).CombinedOutput()
	if err != nil {
		return "", tail(string(loadOut), 4000), fmt.Errorf("loaded archive missing expected image ref: %w", err)
	}
	loadedID := strings.TrimSpace(string(inspectOut))
	if loadedID != src.ScreenedImageID {
		return "", tail(string(loadOut), 4000), fmt.Errorf("screened image id mismatch: expected %s got %s", src.ScreenedImageID, loadedID)
	}
	identity, err := isolatedIdentity()
	if err != nil {
		return "", tail(string(loadOut), 4000), err
	}
	localRef := "dittobench-sub:" + safeTag(src) + "-" + identity
	if out, err := exec.CommandContext(ctx, "docker", "image", "tag", src.ScreenedImageRef, localRef).CombinedOutput(); err != nil {
		return "", tail(string(loadOut), 4000), fmt.Errorf("tag screened image: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// The local request-scoped tag now owns the image. Drop/restore the archive's
	// globally named screener ref immediately; Stop removes localRef after use.
	if err := d.restoreImageRef(ctx, src.ScreenedImageRef, priorID); err != nil {
		return "", tail(string(loadOut), 4000), fmt.Errorf("remove loaded archive ref: %w", err)
	}
	cleanupSourceRef = false
	return localRef, tail(string(loadOut), 2000), nil
}

func validateDockerSaveArchive(path, expectedRef, expectedID string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	expectedHex := strings.TrimPrefix(expectedID, "sha256:")
	expectedClassicConfig := expectedHex + ".json"
	expectedOCIBlob := "blobs/sha256/" + expectedHex
	reader := tar.NewReader(file)
	var manifestBytes []byte
	var indexBytes []byte
	var expectedOCIIndexBytes []byte
	verifiedSmallBlobs := make(map[string]bool)
	smallBlobBytes := make(map[string][]byte)
	smallBlobCount := 0
	smallBlobTotal := int64(0)
	expectedDigestCount := 0
	expectedDigestOK := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read tar: %w", err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		if name == "manifest.json" {
			if manifestBytes != nil {
				return false, fmt.Errorf("duplicate manifest.json")
			}
			if header.Size <= 0 || header.Size > 1<<20 {
				return false, fmt.Errorf("manifest.json has invalid size %d", header.Size)
			}
			manifestBytes, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil {
				return false, fmt.Errorf("read manifest.json: %w", err)
			}
			if int64(len(manifestBytes)) != header.Size {
				return false, fmt.Errorf("read manifest.json: expected %d bytes got %d", header.Size, len(manifestBytes))
			}
			continue
		}
		if name == "index.json" {
			if indexBytes != nil {
				return false, fmt.Errorf("duplicate index.json")
			}
			if header.Size <= 0 || header.Size > 1<<20 {
				return false, fmt.Errorf("index.json has invalid size %d", header.Size)
			}
			indexBytes, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil {
				return false, fmt.Errorf("read index.json: %w", err)
			}
			if int64(len(indexBytes)) != header.Size {
				return false, fmt.Errorf("read index.json: expected %d bytes got %d", header.Size, len(indexBytes))
			}
			continue
		}
		if name == expectedClassicConfig || name == expectedOCIBlob {
			expectedDigestCount++
			if name == expectedOCIBlob {
				if header.Size <= 0 || header.Size > 4<<20 {
					return false, fmt.Errorf("expected OCI index has invalid size %d", header.Size)
				}
				expectedOCIIndexBytes, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
				if err != nil {
					return false, fmt.Errorf("read expected OCI index: %w", err)
				}
				if int64(len(expectedOCIIndexBytes)) != header.Size {
					return false, fmt.Errorf("read expected OCI index: expected %d bytes got %d", header.Size, len(expectedOCIIndexBytes))
				}
				digest := sha256.Sum256(expectedOCIIndexBytes)
				expectedDigestOK = strings.EqualFold(hex.EncodeToString(digest[:]), expectedHex)
				verifiedSmallBlobs[name] = expectedDigestOK
			} else {
				hasher := sha256.New()
				written, err := io.Copy(hasher, io.LimitReader(reader, header.Size+1))
				if err != nil {
					return false, fmt.Errorf("read image config: %w", err)
				}
				if written != header.Size {
					return false, fmt.Errorf("read image config: expected %d bytes got %d", header.Size, written)
				}
				expectedDigestOK = strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), expectedHex)
			}
			continue
		}
		if digest, ok := ociBlobDigest(name); ok && header.Size > 0 && header.Size <= maxAttachmentBlobBytes {
			smallBlobCount++
			smallBlobTotal += header.Size
			if smallBlobCount > 256 || smallBlobTotal > maxAttachmentTotalBytes {
				return false, fmt.Errorf("archive contains too many small OCI blobs")
			}
			blob, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil {
				return false, fmt.Errorf("read OCI blob %s: %w", name, err)
			}
			if int64(len(blob)) != header.Size {
				return false, fmt.Errorf("read OCI blob %s: expected %d bytes got %d", name, header.Size, len(blob))
			}
			got := sha256.Sum256(blob)
			verifiedSmallBlobs[name] = hex.EncodeToString(got[:]) == digest
			if verifiedSmallBlobs[name] {
				smallBlobBytes[name] = blob
			}
		}
	}
	if manifestBytes == nil {
		return false, fmt.Errorf("manifest.json is missing")
	}
	var manifest []struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return false, fmt.Errorf("decode manifest.json: %w", err)
	}
	if len(manifest) != 1 {
		return false, fmt.Errorf("archive must contain exactly one image")
	}
	archiveTagged := len(manifest[0].RepoTags) == 1 && manifest[0].RepoTags[0] == expectedRef
	if len(manifest[0].RepoTags) != 0 && !archiveTagged {
		return false, fmt.Errorf("archive tags must be empty or exactly the expected image ref %q", expectedRef)
	}
	classic := strings.TrimPrefix(manifest[0].Config, "./") == expectedClassicConfig
	manifestConfig := strings.TrimPrefix(manifest[0].Config, "./")
	if !classic && !verifiedSmallBlobs[manifestConfig] {
		return false, fmt.Errorf("Docker manifest config bytes do not match their OCI digest")
	}
	oci := false
	if !classic && indexBytes != nil {
		runnableDigests, err := validatePrimaryOCIIndex(expectedOCIIndexBytes, smallBlobBytes)
		if err != nil {
			return false, err
		}
		var index struct {
			Manifests []ociDescriptor `json:"manifests"`
		}
		if err := json.Unmarshal(indexBytes, &index); err != nil {
			return false, fmt.Errorf("decode index.json: %w", err)
		}
		if len(index.Manifests) == 0 || len(index.Manifests) > 1+maxAttestationDescriptors {
			return false, fmt.Errorf("index.json has an invalid descriptor count %d", len(index.Manifests))
		}
		primaryCount := 0
		for _, descriptor := range index.Manifests {
			if descriptor.Digest == expectedID {
				primaryCount++
				name := strings.TrimPrefix(descriptor.Annotations["io.containerd.image.name"], "docker.io/")
				if descriptor.MediaType != ociImageIndexMediaType ||
					(name != expectedRef && (archiveTagged || name != "")) {
					return false, fmt.Errorf("expected image descriptor has invalid media type or name")
				}
				continue
			}
			if err := validateAttestationDescriptor(descriptor, runnableDigests, smallBlobBytes); err != nil {
				return false, err
			}
		}
		oci = primaryCount == 1
	}
	if !classic && !oci {
		return false, fmt.Errorf("archive index does not bind the expected image id")
	}
	if expectedDigestCount != 1 || !expectedDigestOK {
		return false, fmt.Errorf("archive bytes do not contain the expected image id digest")
	}
	return archiveTagged, nil
}

func validatePrimaryOCIIndex(data []byte, blobs map[string][]byte) (map[string]struct{}, error) {
	var index struct {
		SchemaVersion int             `json:"schemaVersion"`
		MediaType     string          `json:"mediaType"`
		Manifests     []ociDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decode expected OCI index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != ociImageIndexMediaType ||
		len(index.Manifests) == 0 || len(index.Manifests) > 1+maxAttestationDescriptors {
		return nil, fmt.Errorf("expected OCI index has invalid shape")
	}
	runnable := make(map[string]struct{})
	runnableCount := 0
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			continue
		}
		if descriptor.MediaType != ociImageManifestMediaType || descriptor.Size <= 0 ||
			descriptor.Platform == nil || descriptor.Platform.Architecture == "unknown" || descriptor.Platform.OS == "unknown" ||
			!canonicalSHA256Digest(descriptor.Digest) {
			return nil, fmt.Errorf("expected OCI index contains an invalid runnable descriptor")
		}
		runnable[descriptor.Digest] = struct{}{}
		runnableCount++
	}
	if runnableCount != 1 || len(runnable) != 1 {
		return nil, fmt.Errorf("expected OCI index must contain exactly one runnable image")
	}
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations["vnd.docker.reference.type"] != "attestation-manifest" {
			continue
		}
		if err := validateAttestationDescriptor(descriptor, runnable, blobs); err != nil {
			return nil, err
		}
	}
	return runnable, nil
}

func ociBlobDigest(name string) (string, bool) {
	const prefix = "blobs/sha256/"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	digest := strings.TrimPrefix(name, prefix)
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		return "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", false
	}
	return digest, true
}

func validateAttestationDescriptor(descriptor ociDescriptor, runnableDigests map[string]struct{}, blobs map[string][]byte) error {
	if descriptor.MediaType != ociImageManifestMediaType || descriptor.Size <= 0 || descriptor.Size > maxAttachmentBlobBytes {
		return fmt.Errorf("index.json contains a non-attestation descriptor besides the expected image")
	}
	reference := ""
	switch {
	case descriptor.Annotations["vnd.docker.reference.type"] == "attestation-manifest":
		if descriptor.Platform == nil || descriptor.Platform.Architecture != "unknown" || descriptor.Platform.OS != "unknown" {
			return fmt.Errorf("Docker attestation descriptor must target unknown/unknown")
		}
		reference = descriptor.Annotations["vnd.docker.reference.digest"]
	case descriptor.Annotations["io.containerd.manifest.subject"] != "":
		if descriptor.Platform != nil && (descriptor.Platform.Architecture != "unknown" || descriptor.Platform.OS != "unknown") {
			return fmt.Errorf("containerd attestation descriptor has a runnable platform")
		}
		reference = descriptor.Annotations["io.containerd.manifest.subject"]
	default:
		return fmt.Errorf("index.json contains a non-attestation descriptor besides the expected image")
	}
	if !canonicalSHA256Digest(descriptor.Digest) || !canonicalSHA256Digest(reference) {
		return fmt.Errorf("index.json attestation descriptor has a malformed digest")
	}
	if _, ok := runnableDigests[reference]; !ok {
		return fmt.Errorf("index.json attestation does not reference the sole runnable image")
	}
	if name := descriptor.Annotations["io.containerd.image.name"]; name != "" {
		return fmt.Errorf("index.json attestation descriptor must not carry an image name")
	}
	return validateAttestationManifest(descriptor, reference, blobs)
}

func validateAttestationManifest(descriptor ociDescriptor, runnableDigest string, blobs map[string][]byte) error {
	manifestPath := "blobs/sha256/" + strings.TrimPrefix(descriptor.Digest, "sha256:")
	manifestBytes := blobs[manifestPath]
	if int64(len(manifestBytes)) != descriptor.Size {
		return fmt.Errorf("attestation manifest bytes are missing or size-mismatched")
	}
	var manifest struct {
		SchemaVersion int             `json:"schemaVersion"`
		MediaType     string          `json:"mediaType"`
		Config        ociDescriptor   `json:"config"`
		Layers        []ociDescriptor `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.SchemaVersion != 2 ||
		manifest.MediaType != ociImageManifestMediaType || manifest.Config.MediaType != ociImageConfigMediaType ||
		!canonicalSHA256Digest(manifest.Config.Digest) || manifest.Config.Size <= 0 || len(manifest.Layers) != 1 {
		return fmt.Errorf("attached manifest is not a bounded OCI attestation")
	}
	layer := manifest.Layers[0]
	if layer.MediaType != inTotoMediaType || !canonicalSHA256Digest(layer.Digest) || layer.Size <= 0 ||
		layer.Size > maxAttachmentBlobBytes || layer.Annotations["in-toto.io/predicate-type"] == "" {
		return fmt.Errorf("attached manifest does not contain exactly one in-toto layer")
	}
	configBytes := blobs["blobs/sha256/"+strings.TrimPrefix(manifest.Config.Digest, "sha256:")]
	layerBytes := blobs["blobs/sha256/"+strings.TrimPrefix(layer.Digest, "sha256:")]
	if int64(len(configBytes)) != manifest.Config.Size || int64(len(layerBytes)) != layer.Size {
		return fmt.Errorf("attestation config or layer bytes are missing")
	}
	var config struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}
	if err := json.Unmarshal(configBytes, &config); err != nil || config.Architecture != "unknown" || config.OS != "unknown" {
		return fmt.Errorf("attestation config is not unknown/unknown")
	}
	var statement struct {
		Subject []struct {
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string `json:"predicateType"`
	}
	if err := json.Unmarshal(layerBytes, &statement); err != nil || statement.PredicateType != layer.Annotations["in-toto.io/predicate-type"] {
		return fmt.Errorf("in-toto statement predicate does not match its descriptor")
	}
	wantHex := strings.TrimPrefix(runnableDigest, "sha256:")
	for _, subject := range statement.Subject {
		if subject.Digest["sha256"] == wantHex {
			return nil
		}
	}
	return fmt.Errorf("in-toto statement does not bind the runnable image")
}

func canonicalSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func (d *LocalDocker) inspectImageID(ctx context.Context, ref string) string {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (d *LocalDocker) restoreImageRef(ctx context.Context, ref, priorID string) error {
	if priorID == "" {
		out, err := exec.CommandContext(ctx, "docker", "image", "rm", ref).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker image rm %s: %s: %w", ref, strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "image", "tag", priorID, ref).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restore docker image ref %s: %s: %w", ref, strings.TrimSpace(string(out)), err)
	}
	return nil
}
