package sandbox

import (
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
		"--tmpfs":  "/tmp:rw,noexec,nosuid,nodev,size=256m",
		"--ulimit": "nofile=1024:1024",
	} {
		if !hasFlagPair(args, flag, value) {
			t.Errorf("expected %s %s, got %v", flag, value, args)
		}
	}
	if !slices.Contains(args, "--read-only") || !slices.Contains(args, "--init") {
		t.Errorf("expected read-only rootfs and init, got %v", args)
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
