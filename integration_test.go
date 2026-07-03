//go:build integration

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBinaryBuilds(t *testing.T) {
	cmd := exec.Command("go", "build", "-o", "bin/protomolecule-test", ".")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("binary failed to build: %v\n%s", err, string(out))
	}
	defer os.Remove("bin/protomolecule-test")
}

func TestVersionExitsZero(t *testing.T) {
	// Build first
	buildCmd := exec.Command("go", "build", "-o", "bin/protomolecule-test", ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, string(out))
	}
	defer os.Remove("bin/protomolecule-test")

	cmd := exec.Command("./bin/protomolecule-test", "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--version exited with error: %v\n%s", err, string(out))
	}
	output := strings.TrimSpace(string(out))
	if !strings.Contains(output, "0.1.0") {
		t.Errorf("expected version output to contain '0.1.0', got: %q", output)
	}
}

func TestHelpExitsZero(t *testing.T) {
	buildCmd := exec.Command("go", "build", "-o", "bin/protomolecule-test", ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, string(out))
	}
	defer os.Remove("bin/protomolecule-test")

	cmd := exec.Command("./bin/protomolecule-test", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--help exited with error: %v\n%s", err, string(out))
	}
	output := string(out)
	if !strings.Contains(output, "protomolecule") {
		t.Errorf("expected help output to contain 'protomolecule', got: %q", output)
	}
}

func TestStatusWithNoDaemonReturnsError(t *testing.T) {
	buildCmd := exec.Command("go", "build", "-o", "bin/protomolecule-test", ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, string(out))
	}
	defer os.Remove("bin/protomolecule-test")

	cmd := exec.Command("./bin/protomolecule-test", "status")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected status with no daemon to exit with error, but it succeeded")
	}
	output := string(out)
	// Should show a connection error, not a panic
	if strings.Contains(output, "panic") {
		t.Errorf("status command panicked instead of returning clean error:\n%s", output)
	}
}
