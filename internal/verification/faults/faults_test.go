package faults

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStorageFaultRulesCoverEveryScopedPath(t *testing.T) {
	for fault, primitive := range StorageFaults {
		rules, err := StorageFaultRules(fault)
		if err != nil {
			t.Fatalf("%s: %v", fault, err)
		}
		if len(rules) != len(StoragePaths) {
			t.Fatalf("%s: %d rules, want one per scoped path", fault, len(rules))
		}
		for _, rule := range rules {
			if !strings.Contains(rule, primitive.Operation+":") {
				t.Fatalf("%s: rule %q does not scope %s", fault, rule, primitive.Operation)
			}
		}
	}
	if _, err := StorageFaultRules("chmod"); err == nil {
		t.Fatal("unknown storage fault accepted")
	}
}

func TestToxicPayloadsFrozenMagnitudes(t *testing.T) {
	latency := toxicFor("latency")
	if latency.Type != "latency" || latency.Attributes["latency"] != 600 {
		t.Fatalf("latency toxic drifted: %+v", latency)
	}
	reset := toxicFor("reset_peer")
	if reset.Type != "reset_peer" || len(reset.Attributes) != 0 {
		t.Fatalf("reset_peer toxic drifted: %+v", reset)
	}
	bandwidth := toxicFor("bandwidth")
	if bandwidth.Type != "bandwidth" || bandwidth.Attributes["rate"] != 16 {
		t.Fatalf("bandwidth toxic drifted: %+v", bandwidth)
	}
	limit := toxicFor("limit_data")
	if limit.Type != "limit_data" || limit.Attributes["bytes"] != 2048 {
		t.Fatalf("limit_data toxic drifted: %+v", limit)
	}
}

// TestToxiproxyFaultVocabularyObserved drives the real digest-pinned
// Toxiproxy through every closed TCP primitive inside the in-network
// rig. It runs only when the caller opts in (the acceptance path and CI
// own the full run) so `go test ./...` stays fast.
func TestToxiproxyFaultVocabularyObserved(t *testing.T) {
	if os.Getenv("QUOIN_T40_FAULTS_E2E") == "" {
		t.Skip("QUOIN_T40_FAULTS_E2E not set; toxiproxy primitive run disabled")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	arch := dockerServerArch(t)
	if arch == "" {
		t.Skip("docker server architecture unresolvable")
	}
	client := filepath.Join(t.TempDir(), "faultclient")
	if err := BuildFaultclient(client, arch, root); err != nil {
		t.Fatal(err)
	}
	rig, err := StartNetworkRig("quoin-t40-tcp-test", client, "alpine:3.20", t.TempDir(), 28474)
	if err != nil {
		t.Skipf("network rig unavailable: %v", err)
	}
	defer func() {
		rig.Stop()
		if !rig.Removed() {
			t.Errorf("rig left residue behind")
		}
	}()

	for _, fault := range TCPFaults {
		fault := fault
		t.Run(fault, func(t *testing.T) {
			observation, err := rig.ObserveTCPFault(fault)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(observation)
			t.Logf("%s", body)
			if observation.ClientClass != "fault_deterministic_"+fault {
				t.Fatalf("class %q: %+v", observation.ClientClass, observation)
			}
		})
	}
	if !rig.RoutesRestored() {
		t.Fatal("proxy routes did not return to the baseline envelope after toxic removal")
	}
}

// TestFaultfsStoragePrimitivesNative drives every storage fault
// primitive inside a privileged container per fault when the host can
// run one (docker with /dev/fuse passthrough). This is the same cell
// execution the storage-faults suite performs.
func TestFaultfsStoragePrimitivesNative(t *testing.T) {
	if os.Getenv("QUOIN_T40_FAULTS_E2E") == "" {
		t.Skip("QUOIN_T40_FAULTS_E2E not set; faultfs primitive run disabled")
	}
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/fuse"); err != nil {
			t.Skip("/dev/fuse not available")
		}
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	serverArch := dockerServerArch(t)
	if serverArch == "" {
		t.Skip("docker server architecture unresolvable")
	}
	binary := filepath.Join(t.TempDir(), "quoin-faultfs")
	if err := BuildFaultfs(binary, serverArch, root); err != nil {
		t.Fatal(err)
	}
	faultfs := &Faultfs{BinaryPath: binary, Workdir: t.TempDir(), Container: "quoin-t40-faultfs-test", Image: "alpine:3.20"}
	for fault := range StorageFaults {
		fault := fault
		t.Run(fault, func(t *testing.T) {
			outcome, err := faultfs.RunStorageFaultCell(fault)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(outcome)
			t.Logf("%s", body)
			if outcome.Class != "fault_deterministic_"+fault {
				t.Fatalf("class %q: %+v", outcome.Class, outcome)
			}
		})
	}
}

// dockerServerArch resolves the docker server architecture ("amd64" or
// "arm64"); empty when docker is unusable.
func dockerServerArch(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return ""
	}
	arch := strings.TrimSpace(string(output))
	if arch != "amd64" && arch != "arm64" && arch != "x86_64" && arch != "aarch64" {
		return ""
	}
	switch arch {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return arch
}
