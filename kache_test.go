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

func TestKachePin(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			a := kacheAsset(goos, goarch)
			tag, want, err := kachePin(a, "", "")
			if err != nil || tag != kacheVersion || len(want) != 64 {
				t.Errorf("%s/%s: %s %s %v", goos, goarch, tag, want, err)
			}
		}
	}
	if kacheAsset("windows", "amd64") != "" {
		t.Error("windows")
	}
	a := kacheAsset("linux", "amd64")
	if _, _, err := kachePin(a, "", strings.Repeat("0", 64)); err == nil {
		t.Error("aceptó otro hash para la versión fijada")
	}
	if _, _, err := kachePin(a, "v9.9.9", ""); err == nil {
		t.Error("otra versión sin hash")
	}
	if tag, want, err := kachePin(a, "v9.9.9", strings.Repeat("AB", 32)); err != nil || tag != "v9.9.9" || want != strings.Repeat("ab", 32) {
		t.Errorf("otra versión con hash: %s %s %v", tag, want, err)
	}
	if _, want, err := kachePin(a, "v9.9.9", "release"); err != nil || want != "release" {
		t.Errorf("release: %s %v", want, err)
	}
	for _, bad := range []string{"9.9.9", "v1/../x", "v1 x"} {
		if _, _, err := kachePin(a, bad, "release"); err == nil {
			t.Errorf("versión %q aceptada", bad)
		}
	}
	if _, _, err := kachePin(a, "v9.9.9", "xyz"); err == nil {
		t.Error("hash inválido aceptado")
	}
}
