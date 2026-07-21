package sandbox

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// hasFlagPair reports whether args contains the adjacent pair [flag, value].
func hasFlagPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestRunArgs_DefaultsHardenAndBound(t *testing.T) {
	d := NewLocalDocker()
	args := d.runArgs("img:latest", nil)

	// Always-on isolation: a pids bound, no-new-privileges, loopback publish.
	if !hasFlagPair(args, "--pids-limit", "512") {
		t.Errorf("expected --pids-limit 512, got %v", args)
	}
	if !hasFlagPair(args, "--security-opt", "no-new-privileges") {
		t.Errorf("expected --security-opt no-new-privileges, got %v", args)
	}
	if !hasFlagPair(args, "--publish", "127.0.0.1:0:8080") {
		t.Errorf("expected loopback publish, got %v", args)
	}
	// Runtime isolation is default-on even outside the production Compose stack.
	if slices.Contains(args, "--network") {
		t.Errorf("default must not attach an egress network, got %v", args)
	}
	if !hasFlagPair(args, "--cap-drop", "ALL") {
		t.Errorf("default must drop all capabilities, got %v", args)
	}
	for flag, value := range map[string]string{
		"--user":   "65532:65532",
		"--tmpfs":  "/tmp:rw,noexec,nosuid,nodev,size=512m",
		"--memory": "3g",
		"--cpus":   "2",
		"--ulimit": "nofile=1024:1024",
	} {
		if !hasFlagPair(args, flag, value) {
			t.Errorf("expected %s %s, got %v", flag, value, args)
		}
	}
	if !slices.Contains(args, "--read-only") || !slices.Contains(args, "--init") {
		t.Errorf("expected read-only rootfs and init, got %v", args)
	}
	if slices.Contains(args, "--rm") {
		t.Errorf("--rm would erase OOM evidence before diagnostics: %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "HTTPS_PROXY=") || strings.HasPrefix(a, "HTTP_PROXY=") {
			t.Errorf("default must not inject a proxy, got %v", args)
		}
	}
	// The image is last.
	if args[len(args)-1] != "img:latest" {
		t.Errorf("image must be the final arg, got %v", args)
	}
}

func TestDiagnosticsCapturesSanitizedResourceEvidence(t *testing.T) {
	d := NewLocalDocker()
	d.dockerCommand = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "inspect":
			return []byte(`{"Running":true,"OOMKilled":false,"ExitCode":0}`), nil
		case "exec":
			return []byte("__memory_events__\nlow 0\nhigh 0\nmax 2\noom 1\noom_kill 1\n__memory_peak__\n3221225472\n__tmpfs__\ntmpfs 524288 524288 0 100% /tmp\n"), nil
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return nil, nil
		}
	}

	diagnostics := d.Diagnostics(context.Background(), &Handle{ContainerID: "opaque"})
	if diagnostics.InfrastructureCode() != "sandbox_oom" {
		t.Fatalf("expected sandbox_oom, got %+v", diagnostics)
	}
	if diagnostics.MemoryPeakBytes == nil || *diagnostics.MemoryPeakBytes != 3221225472 {
		t.Fatalf("memory peak missing: %+v", diagnostics)
	}
	if diagnostics.TmpfsUsedBytes == nil || *diagnostics.TmpfsUsedBytes != 512<<20 {
		t.Fatalf("tmpfs usage missing: %+v", diagnostics)
	}
	if diagnostics.TmpfsCapacityBytes == nil || *diagnostics.TmpfsCapacityBytes != 512<<20 {
		t.Fatalf("tmpfs capacity missing: %+v", diagnostics)
	}
}

func TestDiagnosticsNonOOMExitIsNotInfrastructure(t *testing.T) {
	d := NewLocalDocker()
	d.dockerCommand = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"Running":false,"OOMKilled":false,"ExitCode":2}`), nil
	}
	diagnostics := d.Diagnostics(context.Background(), &Handle{ContainerID: "opaque"})
	if diagnostics.ExitCode != 2 || diagnostics.InfrastructureCode() != "" {
		t.Fatalf("ordinary exit must remain non-infrastructure: %+v", diagnostics)
	}
}

func TestRunArgs_ZeroValuePidsStillBounded(t *testing.T) {
	// A directly-constructed LocalDocker (PidsLimit == 0) must not become
	// --pids-limit 0 (which docker treats as UNLIMITED).
	d := &LocalDocker{HarnessPort: "8080", MemoryLimit: "2g", CPULimit: "2"}
	if !hasFlagPair(d.runArgs("img", nil), "--pids-limit", "512") {
		t.Errorf("zero PidsLimit must default to 512, not 0/unlimited")
	}
}

func TestRunArgs_HardenDropsCaps(t *testing.T) {
	d := NewLocalDocker()
	d.Harden = true
	if !hasFlagPair(d.runArgs("img", nil), "--cap-drop", "ALL") {
		t.Errorf("Harden must add --cap-drop ALL")
	}
}

func TestRunArgs_CanaryHasNoDockerOrHostMount(t *testing.T) {
	d := NewLocalDocker()
	args := d.runArgs("security-canary:latest", map[string]string{"CANARY": "inert"})
	for _, forbidden := range []string{"--privileged", "--pid=host", "--ipc=host", "--network=host", "/var/run/docker.sock", "/:/"} {
		if slices.Contains(args, forbidden) || strings.Contains(strings.Join(args, " "), forbidden) {
			t.Fatalf("runtime arguments exposed forbidden host surface %q: %v", forbidden, args)
		}
	}
}

func TestRunArgs_ExplicitLSMProfiles(t *testing.T) {
	d := NewLocalDocker()
	d.SeccompProfile = "/etc/ditto/seccomp.json"
	d.AppArmorProfile = "ditto-untrusted"
	args := d.runArgs("img", nil)
	if !hasFlagPair(args, "--security-opt", "seccomp=/etc/ditto/seccomp.json") {
		t.Fatalf("seccomp profile missing: %v", args)
	}
	if !hasFlagPair(args, "--security-opt", "apparmor=ditto-untrusted") {
		t.Fatalf("AppArmor profile missing: %v", args)
	}
}

func TestRunArgs_EgressNetworkAttached(t *testing.T) {
	d := NewLocalDocker()
	d.EgressNetwork = "ditto-sandbox"
	if !hasFlagPair(d.runArgs("img", nil), "--network", "ditto-sandbox") {
		t.Errorf("EgressNetwork must attach --network")
	}
}

func TestRunArgs_EgressProxyInjectsEnv(t *testing.T) {
	d := NewLocalDocker()
	d.EgressProxy = "http://egress:3128"
	args := d.runArgs("img", map[string]string{"OPENROUTER_API_KEY": "sk-x"})

	if !hasFlagPair(args, "-e", "HTTPS_PROXY=http://egress:3128") {
		t.Errorf("EgressProxy must inject HTTPS_PROXY, got %v", args)
	}
	if !hasFlagPair(args, "-e", "HTTP_PROXY=http://egress:3128") {
		t.Errorf("EgressProxy must inject HTTP_PROXY, got %v", args)
	}
	// Loopback + host gateway must bypass the proxy.
	if !hasFlagPair(args, "-e", "NO_PROXY=host.docker.internal,localhost,127.0.0.1") {
		t.Errorf("EgressProxy must inject NO_PROXY, got %v", args)
	}
	// The caller's env still rides along.
	if !hasFlagPair(args, "-e", "OPENROUTER_API_KEY=sk-x") {
		t.Errorf("caller env must be preserved, got %v", args)
	}
}

func TestCleanupStaleRemovesOnlyOwnedExplicitResources(t *testing.T) {
	d := NewLocalDocker()
	var calls [][]string
	d.dockerCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case reflect.DeepEqual(args, []string{"ps", "-aq", "--filter", "label=io.heyditto.dittobench.run"}):
			return []byte("container-a\ncontainer-b\n"), nil
		case reflect.DeepEqual(args, []string{"rm", "-f", "container-a", "container-b"}):
			return nil, nil
		case reflect.DeepEqual(args, []string{"network", "ls", "-q", "--filter", "label=io.heyditto.dittobench.run"}):
			return []byte("network-a\nnetwork-b\n"), nil
		case reflect.DeepEqual(args, []string{"network", "rm", "network-a"}),
			reflect.DeepEqual(args, []string{"network", "rm", "network-b"}):
			return nil, nil
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return nil, nil
		}
	}

	if err := d.CleanupStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatalf("expected five explicit docker calls, got %v", calls)
	}
}
