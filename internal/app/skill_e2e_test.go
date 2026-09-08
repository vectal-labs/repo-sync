package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the shipped executable outside the checkout, with no network, services,
// or agent commands involved. This catches missing embedded release assets.
func TestBundledSkillCLIOutsideCheckout(t *testing.T) {
	binaryBytes := uninstallBinaryBytes(t)
	_, target := skillFixture(t)
	binary := filepath.Join(t.TempDir(), "repo-sync")
	if err := os.WriteFile(binary, binaryBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".claude", ".hermes"} {
		if err := os.Mkdir(filepath.Join(os.Getenv("HOME"), dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "my-other-skill", "SKILL.md")
	skillWrite(t, unrelated, "keep my other skill")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = filepath.Dir(binary)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	if !strings.Contains(run("help"), "skill <command>") {
		t.Fatal("missing CLI discovery")
	}
	run("skill", "install")
	for _, path := range []string{"SKILL.md", "references/operations.md"} {
		installed, err := os.ReadFile(filepath.Join(target, path))
		if err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(filepath.Join("../..", ".agents", "skills", "repo-sync", path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(installed, source) {
			t.Fatalf("bundled %s differs from source", path)
		}
	}
	if got := run("skill", "status"); strings.Count(got, "installed; current") != 3 {
		t.Fatal(got)
	}
	run("skill", "refresh")
	run("skill", "uninstall")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("skill still exists: %v", err)
	}
	if got := skillRead(t, unrelated); got != "keep my other skill" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")); !os.IsNotExist(err) {
		t.Fatal("skill command touched services")
	}
}
