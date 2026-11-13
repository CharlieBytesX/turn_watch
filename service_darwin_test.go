//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// El LaunchAgent es un plist válido (plutil), también con rutas raras.
func TestLaunchPlist(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "a & <b>"))
	p := filepath.Join(t.TempDir(), "x.plist")
	if err := os.WriteFile(p, []byte(launchPlist("/Users/x & y/.local/bin/turn_watch")), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", p).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v\n%s", err, out)
	}
}
