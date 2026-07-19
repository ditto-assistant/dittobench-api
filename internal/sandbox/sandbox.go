// Package sandbox builds and runs an untrusted miner harness in an isolated
// container, mirroring the SN118 on-chain validator's "build the submitted
// harness, run it in a Docker sandbox, score it, tear it down" loop — minus
// the chain.
//
// The Sandbox interface lets the API swap execution backends without touching
// the submit handler:
//
//   - LocalDocker (this file): `docker build` + `docker run` on the host
//     daemon. Good for self-hosting the practice validator and for local dev.
//   - CloudBuild (future, for the Cloud Run deployment): Cloud Build to build
//     the image + a Cloud Run Job to run it. Cloud Run has no local Docker
//     daemon, so the LocalDocker backend cannot run there; see README.
//
// Isolation: resource caps (memory/cpu/pids) + auto-remove + no-new-privileges,
// plus opt-in hardening for untrusted (on-chain) submissions — `--cap-drop ALL`
// (Harden) and an egress allowlist via a restricted network + forward proxy
// (EgressNetwork/EgressProxy; see the Sandbox egress section in
// docs/model-lock.md). With the egress config unset it stays "good-faith
// practice" grade (full-egress bridge), which is fine for the miner's own
// practice submissions; the on-chain validator turns the egress + cap-drop
// hardening on and provisions the proxy + host firewall. Deeper isolation
// (seccomp/gVisor/Kata) is a later hardening step.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/astfp"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// Sandbox builds a submission into a runnable image and runs it as an
// addressable harness serving the /run + /health contract.
type Sandbox interface {
	// Available returns nil if the backend can build and run images.
	Available(ctx context.Context) error
	// Build clones src (a git URL at ref) and builds it into an image,
	// returning the image reference and the tail of the build log.
	Build(ctx context.Context, src Source) (image string, buildLog string, fingerprint *protocol.CodeFingerprint, err error)
	// Run starts image as a detached container exposing the harness port and
	// returns a handle. Caller must Stop the handle.
	Run(ctx context.Context, image string, env map[string]string) (*Handle, error)
	// Release removes a request-scoped image returned by Build. It is safe to
	// call more than once and must also be called when work fails before Run.
	Release(ctx context.Context, image string)
	// Stop force-removes the container behind h. Safe to call more than once.
	Stop(ctx context.Context, h *Handle)
	// Diagnostics captures sanitized container resource evidence before Stop.
	// It never returns miner output, environment variables, paths, or source.
	Diagnostics(ctx context.Context, h *Handle) RuntimeDiagnostics
}

// Source identifies a submission to build. Exactly one of GitURL or TarballURL
// is set: GitURL clones a repo; TarballURL fetches a presigned gzipped-tar of
// the harness (the SN118 platform stores miner uploads as tarballs, so the
// validator hands us the platform's short-lived download URL).
type Source struct {
	GitURL     string // e.g. https://github.com/<miner>/<harness>
	GitRef     string // branch, tag, or commit (default: default branch)
	TarballURL string // presigned https URL of a gzipped tar of the harness
	// TarballSHA256, when non-empty, is verified (hex) against the fetched bytes.
	// The platform already checks it at upload; re-verifying makes the sandbox
	// self-defending against a corrupted or swapped blob behind the URL.
	TarballSHA256       string
	ScreenedImageURL    string
	ScreenedImageSHA256 string
	ScreenedImageID     string
	ScreenedImageRef    string
	ScreenedImageSize   int64
}

// Handle references a running harness container.
type Handle struct {
	ContainerID string
	BaseURL     string // e.g. http://127.0.0.1:49160 — pass to runner.RunHarness
	// ImageRef is the request-scoped image created by Build. Stop removes it
	// after removing the container so long-lived validators do not accumulate
	// multi-gigabyte submission images.
	ImageRef string
}

// RuntimeDiagnostics is bounded, source-free evidence captured before teardown.
// Pointer fields distinguish an observed zero from a runtime that could not
// expose the corresponding cgroup/tmpfs counter (for example after a hard exit).
type RuntimeDiagnostics struct {
	StateKnown         bool              `json:"-"`
	Running            bool              `json:"running"`
	OOMKilled          bool              `json:"oom_killed"`
	ExitCode           int               `json:"exit_code"`
	MemoryPeakBytes    *uint64           `json:"memory_peak_bytes,omitempty"`
	MemoryEvents       map[string]uint64 `json:"memory_events,omitempty"`
	TmpfsUsedBytes     *uint64           `json:"tmpfs_used_bytes,omitempty"`
	TmpfsCapacityBytes *uint64           `json:"tmpfs_capacity_bytes,omitempty"`
}

// InfrastructureCode returns a stable retry classification only for evidence
// that the validator-owned resource envelope was exhausted. An ordinary
// non-zero process exit remains a miner/runtime failure.
func (d RuntimeDiagnostics) InfrastructureCode() string {
	if d.OOMKilled || d.MemoryEvents["oom_kill"] > 0 {
		return "sandbox_oom"
	}
	if d.TmpfsUsedBytes != nil && d.TmpfsCapacityBytes != nil &&
		*d.TmpfsCapacityBytes > 0 && *d.TmpfsUsedBytes >= *d.TmpfsCapacityBytes {
		return "sandbox_tmpfs_exhausted"
	}
	return ""
}

// LocalDocker runs submissions on the host Docker daemon via the `docker` CLI.
type LocalDocker struct {
	// HarnessPort is the in-container port the harness serves on (the kit's
	// `serve` binds 8080).
	HarnessPort string
	// MemoryLimit / CPULimit cap the container (docker --memory / --cpus).
	MemoryLimit string
	// TmpfsLimit caps the only writable filesystem mounted at /tmp.
	TmpfsLimit string
	// CPULimit is passed to docker --cpus.
	CPULimit string
	// BuildTimeout bounds a single `docker build` (Rust cold builds are slow).
	BuildTimeout time.Duration
	// GitHubTokenFile, if set, is mounted into the build as the BuildKit secret
	// `gh_token` so the build can fetch the private ditto-harness dependency
	// over HTTPS. No-op once that repo is public. Defaults from the
	// GITHUB_TOKEN_FILE env var.
	GitHubTokenFile string
	// AllowPrivate relaxes the SSRF guard on TarballURL fetches (local dev only,
	// e.g. a minio/localhost presigned URL). Mirrors the submit-handler flag.
	AllowPrivate bool
	// PidsLimit caps the container process/thread count (docker --pids-limit) —
	// a fork-bomb bound. Always applied; defaults to 512.
	PidsLimit int
	// Harden, when true, drops all Linux capabilities (--cap-drop ALL). An
	// untrusted userland HTTP harness needs none. Hardening is on by default;
	// DITTOBENCH_SANDBOX_HARDEN=0 is reserved for local debugging.
	Harden bool
	// SeccompProfile and AppArmorProfile are explicit runtime policy names.
	// Docker's built-in seccomp profile remains active when SeccompProfile is
	// empty. AppArmor is opt-in because it is unavailable on some hosts.
	SeccompProfile  string
	AppArmorProfile string
	// EgressNetwork, when set, attaches the container to this user-defined docker
	// network — the egress-restricted sandbox network (allowlisting proxy + host
	// firewall) — instead of the default full-egress bridge. Empty = today's
	// behavior. Env DITTOBENCH_SANDBOX_EGRESS_NETWORK. See the Sandbox egress
	// section in docs/model-lock.md.
	EgressNetwork string
	// EgressProxy, when set, is injected as HTTPS_PROXY/HTTP_PROXY so the harness's
	// outbound calls are forced through the allowlisting forward proxy (loopback +
	// the host gateway bypass it via NO_PROXY). Env DITTOBENCH_SANDBOX_EGRESS_PROXY.
	EgressProxy string
	// dockerCommand is injectable only for deterministic command/parse tests.
	dockerCommand func(context.Context, ...string) ([]byte, error)
}

// NewLocalDocker returns a LocalDocker with sensible defaults.
func NewLocalDocker() *LocalDocker {
	return &LocalDocker{
		HarnessPort:     "8080",
		MemoryLimit:     "3g",
		TmpfsLimit:      "512m",
		CPULimit:        "2",
		BuildTimeout:    25 * time.Minute,
		GitHubTokenFile: os.Getenv("GITHUB_TOKEN_FILE"),
		AllowPrivate:    envBool("DITTOBENCH_ALLOW_PRIVATE_HARNESS"),
		PidsLimit:       envIntDefault("DITTOBENCH_SANDBOX_PIDS_LIMIT", 512),
		Harden:          envBoolDefault("DITTOBENCH_SANDBOX_HARDEN", true),
		SeccompProfile:  strings.TrimSpace(os.Getenv("DITTOBENCH_SANDBOX_SECCOMP_PROFILE")),
		AppArmorProfile: strings.TrimSpace(os.Getenv("DITTOBENCH_SANDBOX_APPARMOR_PROFILE")),
		EgressNetwork:   strings.TrimSpace(os.Getenv("DITTOBENCH_SANDBOX_EGRESS_NETWORK")),
		EgressProxy:     strings.TrimSpace(os.Getenv("DITTOBENCH_SANDBOX_EGRESS_PROXY")),
	}
}

// envBool reports whether an env var is set to a truthy value.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envBoolDefault(name string, def bool) bool {
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// envIntDefault reads a positive int env var, falling back to def on unset or
// invalid/non-positive input.
func envIntDefault(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Available checks that the docker CLI and daemon are reachable.
func (d *LocalDocker) Available(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker daemon unavailable: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Build materializes the submission in a temp dir, then either loads the
// screener-built image or builds the source with BuildKit. We
// clone ourselves (rather than docker's git-context form) so a PRIVATE
// submission repo can be authenticated with the gh token; docker's remote
// build-context fetch is unauthenticated and 404s on private repos. A mounted
// gh_token BuildKit secret then lets the in-image cargo build fetch the private
// ditto-harness crate over HTTPS.
func (d *LocalDocker) Build(ctx context.Context, src Source) (string, string, *protocol.CodeFingerprint, error) {
	if src.GitURL == "" && src.TarballURL == "" {
		return "", "", nil, fmt.Errorf("sandbox: one of git_url or tarball_url is required")
	}
	ctx, cancel := context.WithTimeout(ctx, d.BuildTimeout)
	defer cancel()

	// 1. Materialize the submission into a throwaway working tree, either by
	//    cloning a git repo or by extracting a presigned tarball.
	workdir, err := os.MkdirTemp("", "dittobench-sub-")
	if err != nil {
		return "", "", nil, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(workdir)

	// The docker build context: the git-clone root, or — for a tarball — the
	// extraction root or the lone top-level directory that holds the Dockerfile.
	contextDir := workdir
	if src.TarballURL != "" {
		cdir, err := d.fetchTarball(ctx, src, workdir)
		if err != nil {
			return "", "", nil, err
		}
		contextDir = cdir
	} else {
		cloneURL, err := d.authedCloneURL(src.GitURL)
		if err != nil {
			return "", "", nil, err
		}
		cloneArgs := []string{"clone", "--depth", "1"}
		if src.GitRef != "" {
			cloneArgs = append(cloneArgs, "--branch", src.GitRef)
		}
		cloneArgs = append(cloneArgs, cloneURL, workdir)
		if out, err := exec.CommandContext(ctx, "git", cloneArgs...).CombinedOutput(); err != nil {
			// Redact a token that may appear in the URL within git's error output.
			return "", "", nil, fmt.Errorf("git clone failed: %s: %w", redact(strings.TrimSpace(string(out))), err)
		}
	}

	// 1b. Structural fingerprint of the materialized crate, for the platform's
	//     anti-copy gate. Computed here (the only place the tree is unpacked and a
	//     Rust parser is available) while contextDir still exists — the deferred
	//     RemoveAll wipes it on return. Best-effort: nil on any problem, never
	//     failing the build over a moderation signal.
	fingerprint := astfp.FromDir(ctx, contextDir)

	// 2. Load the screener-built image when present. The source tree above is
	// still materialized so validators preserve structural anti-copy evidence.
	if src.ScreenedImageURL != "" {
		image, loadLog, err := d.loadScreenedImage(ctx, src, workdir)
		if err != nil {
			return "", loadLog, nil, err
		}
		return image, loadLog, fingerprint, nil
	}

	// 3. Build the local context for legacy/evaluating records without an image.
	image := "dittobench-sub:" + safeTag(src)
	args := []string{"build", "-t", image}
	if d.GitHubTokenFile != "" {
		args = append(args, "--secret", "id=gh_token,src="+d.GitHubTokenFile)
	}
	args = append(args, contextDir)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", tail(buf.String(), 4000), nil, fmt.Errorf("docker build failed: %w", err)
	}
	return image, tail(buf.String(), 2000), fingerprint, nil
}

// authedCloneURL injects the gh token into an https github URL so private
// submission repos can be cloned. Non-github or non-https URLs are returned
// unchanged (relying on ambient git credentials).
func (d *LocalDocker) authedCloneURL(gitURL string) (string, error) {
	if d.GitHubTokenFile == "" {
		return gitURL, nil
	}
	const prefix = "https://github.com/"
	if !strings.HasPrefix(gitURL, prefix) {
		return gitURL, nil
	}
	tok, err := os.ReadFile(d.GitHubTokenFile)
	if err != nil {
		return "", fmt.Errorf("read github token file: %w", err)
	}
	token := strings.TrimSpace(string(tok))
	if token == "" {
		return gitURL, nil
	}
	return "https://x-access-token:" + token + "@github.com/" + strings.TrimPrefix(gitURL, prefix), nil
}

// redact removes an x-access-token credential from a string before logging.
func redact(s string) string {
	if i := strings.Index(s, "x-access-token:"); i >= 0 {
		if j := strings.Index(s[i:], "@"); j >= 0 {
			return s[:i] + "x-access-token:***" + s[i+j:]
		}
	}
	return s
}

// pidsLimit returns the configured --pids-limit, defaulting to 512 so a
// zero-value LocalDocker (e.g. constructed directly in a test) is still bounded
// rather than unlimited (docker treats --pids-limit 0 as unlimited).
func (d *LocalDocker) pidsLimit() int {
	if d.PidsLimit > 0 {
		return d.PidsLimit
	}
	return 512
}

func (d *LocalDocker) tmpfsLimit() string {
	if strings.TrimSpace(d.TmpfsLimit) != "" {
		return d.TmpfsLimit
	}
	return "512m"
}

func (d *LocalDocker) dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	if d.dockerCommand != nil {
		return d.dockerCommand(ctx, args...)
	}
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

// runArgs builds the `docker run` argument vector. Extracted from Run so the
// isolation/egress flags can be unit-tested without a live docker daemon.
func (d *LocalDocker) runArgs(image string, env map[string]string) []string {
	args := []string{
		// Do not use --rm: Docker would erase an OOM-killed container before the
		// scorer can inspect State.OOMKilled/ExitCode. Every successful Run path
		// owns a deferred Stop, which removes it immediately after diagnostics.
		"run", "-d",
		"--init",
		"--user", "65532:65532",
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + d.tmpfsLimit(),
		"--memory", d.MemoryLimit,
		"--cpus", d.CPULimit,
		"--pids-limit", strconv.Itoa(d.pidsLimit()),
		"--ulimit", "nofile=1024:1024",
		"--security-opt", "no-new-privileges",
	}
	if d.Harden {
		// An untrusted userland HTTP harness needs no Linux capabilities.
		args = append(args, "--cap-drop", "ALL")
	}
	if d.SeccompProfile != "" {
		args = append(args, "--security-opt", "seccomp="+d.SeccompProfile)
	}
	if d.AppArmorProfile != "" {
		args = append(args, "--security-opt", "apparmor="+d.AppArmorProfile)
	}
	if d.EgressNetwork != "" {
		// The egress-restricted sandbox network (allowlisting proxy + host
		// firewall) instead of the default full-egress bridge. See the Sandbox
		// egress section in docs/model-lock.md.
		args = append(args, "--network", d.EgressNetwork)
	}
	// Let the harness reach host services (e.g. the Ollama embeddings server) at
	// the documented host.docker.internal name. On Linux Docker this needs an
	// explicit host-gateway mapping — unlike Docker Desktop, which injects it
	// automatically — so the miner's OLLAMA_BASE_URL default resolves.
	args = append(args,
		"--add-host", "host.docker.internal:host-gateway",
		"--publish", "127.0.0.1:0:"+d.HarnessPort, // random host port, loopback only
	)
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	if d.EgressProxy != "" {
		// Force the harness's outbound calls through the allowlisting proxy; the
		// host gateway (Ollama) + loopback bypass it via NO_PROXY.
		args = append(args,
			"-e", "HTTPS_PROXY="+d.EgressProxy,
			"-e", "HTTP_PROXY="+d.EgressProxy,
			"-e", "NO_PROXY=host.docker.internal,localhost,127.0.0.1",
		)
	}
	return append(args, image)
}

type dockerState struct {
	Running   bool `json:"Running"`
	OOMKilled bool `json:"OOMKilled"`
	ExitCode  int  `json:"ExitCode"`
}

// Diagnostics collects Docker state plus best-effort cgroup-v2 and /tmp usage.
// The inner image may be distroless or already stopped, so unavailable counters
// stay nil while the authoritative Docker State remains available.
func (d *LocalDocker) Diagnostics(ctx context.Context, h *Handle) RuntimeDiagnostics {
	diagnostics := RuntimeDiagnostics{}
	if h == nil || h.ContainerID == "" {
		return diagnostics
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if out, err := d.dockerOutput(ctx, "inspect", "--format", "{{json .State}}", h.ContainerID); err == nil {
		var state dockerState
		if json.Unmarshal(bytes.TrimSpace(out), &state) == nil {
			diagnostics.StateKnown = true
			diagnostics.Running = state.Running
			diagnostics.OOMKilled = state.OOMKilled
			diagnostics.ExitCode = state.ExitCode
		}
	}
	if !diagnostics.Running {
		return diagnostics
	}

	const script = `
printf '%s\n' __memory_events__
cat /sys/fs/cgroup/memory.events 2>/dev/null || true
printf '%s\n' __memory_peak__
cat /sys/fs/cgroup/memory.peak 2>/dev/null || true
printf '%s\n' __tmpfs__
df -Pk /tmp 2>/dev/null | tail -n 1 || true
`
	out, err := d.dockerOutput(ctx, "exec", h.ContainerID, "/bin/sh", "-c", script)
	if err != nil {
		return diagnostics
	}
	parseRuntimeMetrics(&diagnostics, string(out))
	return diagnostics
}

func parseRuntimeMetrics(diagnostics *RuntimeDiagnostics, output string) {
	section := ""
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		switch line {
		case "__memory_events__", "__memory_peak__", "__tmpfs__":
			section = line
			continue
		}
		if line == "" {
			continue
		}
		switch section {
		case "__memory_events__":
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				continue
			}
			if diagnostics.MemoryEvents == nil {
				diagnostics.MemoryEvents = make(map[string]uint64)
			}
			diagnostics.MemoryEvents[fields[0]] = value
		case "__memory_peak__":
			value, err := strconv.ParseUint(line, 10, 64)
			if err == nil {
				diagnostics.MemoryPeakBytes = &value
			}
		case "__tmpfs__":
			fields := strings.Fields(line)
			if len(fields) < 6 {
				continue
			}
			capacityKB, capErr := strconv.ParseUint(fields[len(fields)-5], 10, 64)
			usedKB, usedErr := strconv.ParseUint(fields[len(fields)-4], 10, 64)
			if capErr == nil && usedErr == nil {
				capacity := capacityKB * 1024
				used := usedKB * 1024
				diagnostics.TmpfsCapacityBytes = &capacity
				diagnostics.TmpfsUsedBytes = &used
			}
		}
	}
}

// Run starts the image detached with resource caps and a random host port, then
// resolves the mapped host port.
func (d *LocalDocker) Run(ctx context.Context, image string, env map[string]string) (*Handle, error) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(runCtx, "docker", d.runArgs(image, env)...).CombinedOutput()
	if err != nil {
		d.Release(context.Background(), image)
		return nil, fmt.Errorf("docker run failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	containerID := strings.TrimSpace(string(out))

	hostPort, err := d.mappedPort(runCtx, containerID)
	if err != nil {
		d.Stop(context.Background(), &Handle{ContainerID: containerID, ImageRef: image})
		return nil, err
	}
	return &Handle{
		ContainerID: containerID,
		BaseURL:     "http://127.0.0.1:" + hostPort,
		ImageRef:    image,
	}, nil
}

// mappedPort returns the host port docker assigned to the harness port.
func (d *LocalDocker) mappedPort(ctx context.Context, containerID string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "port", containerID, d.HarnessPort).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker port: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// Output like "127.0.0.1:49160" (possibly multiple lines for v4/v6).
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	idx := strings.LastIndex(first, ":")
	if idx < 0 {
		return "", fmt.Errorf("unexpected docker port output: %q", first)
	}
	return strings.TrimSpace(first[idx+1:]), nil
}

// Stop force-removes the container and its request-scoped image tag. Errors are
// ignored (best-effort cleanup); removing the container first lets Docker reclaim
// the image layers when no concurrent run still references them.
func (d *LocalDocker) Stop(ctx context.Context, h *Handle) {
	if h == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if h.ContainerID != "" {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", h.ContainerID).Run()
	}
	d.Release(ctx, h.ImageRef)
}

func (d *LocalDocker) Release(ctx context.Context, image string) {
	// Only delete images in the namespace Build owns. Run remains safe to use
	// with an operator-managed image without unexpectedly deleting it.
	if !strings.HasPrefix(image, "dittobench-sub:") {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_ = exec.CommandContext(cleanupCtx, "docker", "image", "rm", image).Run()
}

// safeTag derives a docker-safe tag from the source ref.
func safeTag(src Source) string {
	base := src.GitURL
	if base == "" {
		// Tarball submissions: tag off the URL path (minus query/signature).
		base = src.TarballURL
		if i := strings.IndexByte(base, '?'); i >= 0 {
			base = base[:i]
		}
	}
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".git")
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".tgz")
	var suffix string
	switch {
	case src.GitRef != "":
		suffix = "-" + src.GitRef
	case src.TarballSHA256 != "":
		// Pin the tag to the content hash so re-runs of the same blob reuse the
		// build cache and distinct blobs never collide on a tag.
		if len(src.TarballSHA256) >= 12 {
			suffix = "-" + src.TarballSHA256[:12]
		} else {
			suffix = "-" + src.TarballSHA256
		}
	}
	// A screened image is already content-addressed. Include its image ID even
	// when the practice caller omitted tarball_sha256, preventing two archives
	// with the same URL basename from sharing a mutable local tag.
	if id := strings.TrimPrefix(src.ScreenedImageID, "sha256:"); id != "" {
		if len(id) > 12 {
			id = id[:12]
		}
		suffix += "-image-" + id
	}
	base += suffix
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	t := strings.Trim(b.String(), "-._")
	if t == "" {
		t = "submission"
	}
	if len(t) > 100 {
		// Preserve the content-addressed suffix when long presigned object names
		// would otherwise truncate it away.
		cleanSuffix := strings.TrimLeft(safeTagPart(suffix), "-._")
		if cleanSuffix != "" && len(cleanSuffix) < 99 {
			t = strings.TrimRight(t[:99-len(cleanSuffix)], "-._") + "-" + cleanSuffix
		} else {
			t = t[:100]
		}
	}
	return t
}

func safeTagPart(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// tail returns the last n bytes of s, prefixed with an ellipsis if truncated.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
