package main

// Pruebas contra el sistema de verdad (Linux y macOS; en macOS las corre el CI):
// lectura de procesos, cwd, pausa con señales y la FIFO del jobserver.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadProcsSelf(t *testing.T) {
	// Algo de CPU para que el tiempo acumulado no sea 0.
	for end := time.Now().Add(150 * time.Millisecond); time.Now().Before(end); {
	}
	procs := readProcs()
	me := procs[os.Getpid()]
	if me == nil {
		t.Fatalf("no aparece el propio proceso entre %d", len(procs))
	}
	if me.ppid != os.Getppid() {
		t.Errorf("ppid %d, quería %d", me.ppid, os.Getppid())
	}
	if me.uid != os.Getuid() {
		t.Errorf("uid %d, quería %d", me.uid, os.Getuid())
	}
	base := filepath.Base(os.Args[0])
	if me.comm == "" || !strings.HasPrefix(base, me.comm) {
		t.Errorf("comm %q, quería un prefijo de %q", me.comm, base)
	}
	if args := me.loadArgs(); len(args) == 0 || filepath.Base(args[0]) != base {
		t.Errorf("args %q", args)
	}
	if me.rssKB <= 0 {
		t.Errorf("rss %d KB", me.rssKB)
	}
	if me.own == 0 {
		t.Errorf("sin tiempo de CPU")
	}
	if st := startOf(os.Getpid()); st == 0 || st != me.start {
		t.Errorf("startOf %d, readProcs %d", st, me.start)
	}
	started := bootTime().Add(time.Duration(float64(me.start) / clkTck * float64(time.Second)))
	if d := time.Since(started); d < 0 || d > 10*time.Minute {
		t.Errorf("inicio raro: hace %v", d)
	}
	wd, _ := os.Getwd()
	if cwd := procCwd(me, true); cwd != wd {
		t.Errorf("cwd %q, quería %q", cwd, wd)
	}
	if lite := readProcsLite()[os.Getpid()]; lite == nil || lite.start != me.start || lite.ppid != me.ppid {
		t.Errorf("readProcsLite: %+v", lite)
	}
	if n := numCPU(); n < 1 {
		t.Errorf("numCPU %d", n)
	}
	if used, tot := memInfo(); tot <= 0 || used <= 0 || used > tot {
		t.Errorf("memoria %d / %d MB", used, tot)
	}
	if l := loadAvg(); l < 0 {
		t.Errorf("carga %v", l)
	}
}

// El cwd de otro proceso (en macOS, con lsof) y su pausa con SIGSTOP.
func TestChildCwdAndPause(t *testing.T) {
	dir := t.TempDir()
	c := exec.Command("sleep", "30")
	c.Dir = dir
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	pid := c.Process.Pid

	var p *proc
	for i := 0; i < 20 && (p == nil || p.comm != "sleep"); i++ {
		time.Sleep(50 * time.Millisecond)
		p = readProcs()[pid]
	}
	if p == nil || p.comm != "sleep" || p.ppid != os.Getpid() {
		t.Fatalf("hijo: %+v", p)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got, _ := filepath.EvalSymlinks(procCwd(p, true)); got != want {
		t.Errorf("cwd del hijo %q, quería %q", got, want)
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	paused := false
	for i := 0; i < 40 && !paused; i++ {
		time.Sleep(50 * time.Millisecond)
		if q := readProcsLite()[pid]; q != nil {
			paused = q.paused
		}
	}
	if !paused {
		t.Error("no se ve en pausa tras SIGSTOP")
	}
	_ = syscall.Kill(pid, syscall.SIGCONT)
	for i := 0; i < 40 && paused; i++ {
		time.Sleep(50 * time.Millisecond)
		if q := readProcsLite()[pid]; q != nil {
			paused = q.paused
		}
	}
	if paused {
		t.Error("sigue en pausa tras SIGCONT")
	}
}

// La FIFO del jobserver: mkfifo, escribir turnos y contarlos (FIONREAD).
func TestFifoTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobserver")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("+++")); err != nil {
		t.Fatal(err)
	}
	if n := fifoAvailable(f); n != 3 {
		t.Errorf("%d turnos, quería 3", n)
	}
	if _, err := f.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if n := fifoAvailable(f); n != 2 {
		t.Errorf("%d turnos, quería 2", n)
	}
}
