package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Descarga real desde GitHub a un HOME temporal. Solo con TURN_WATCH_TEST_RED=1.
func TestDownloadKache(t *testing.T) {
	if envVar("TEST_RED") == "" {
		t.Skip("sin red: TURN_WATCH_TEST_RED=1 para probar la descarga")
	}
	t.Setenv("HOME", t.TempDir())
	p, err := downloadKache()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(p, "--version").Output()
	if err != nil || !strings.HasPrefix(string(out), "kache ") {
		t.Fatalf("kache --version: %q %v", out, err)
	}
	t.Logf("%s → %s", p, strings.TrimSpace(string(out)))
}
