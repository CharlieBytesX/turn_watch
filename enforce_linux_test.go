//go:build linux

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Una lista de pausados con un pid que no es un trabajo de compilación (aunque esté
// en pausa y su inicio coincida) no lo reanuda; el archivo se borra igual.
func TestResumeFileValidatesPids(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sin sleep:", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	waitState := func(want byte) bool {
		for i := 0; i < 50; i++ {
			if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
				s := string(b)
				if r := strings.LastIndexByte(s, ')'); r > 0 && len(s) > r+2 && s[r+2] == want {
					return true
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}
	if !waitState('T') {
		t.Fatal("sleep no quedó en pausa")
	}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	b, _ := json.Marshal([]stoppedProc{{PID: pid, Start: startOf(pid)}})
	if err := writeFileSafe(dir, os.Getuid(), "stopped.json", b, 0o600); err != nil {
		t.Fatal(err)
	}
	resumeFile(dir, os.Getuid(), "stopped.json")
	if !waitState('T') {
		t.Fatal("reanudó un proceso que no es un trabajo de compilación")
	}
	if _, err := os.Lstat(filepath.Join(dir, "stopped.json")); !os.IsNotExist(err) {
		t.Errorf("el archivo sigue: %v", err)
	}
}
