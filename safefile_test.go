package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Un enlace simbólico con el nombre del archivo no hace que se escriba en su destino.
func TestWriteFileSafeDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	// El nombre final y un temporal previsible, los dos enlaces al archivo "víctima".
	for _, n := range []string{"stopped-root.json", "stopped-root.json.tmp"} {
		if err := os.Symlink(victim, filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFileSafe(dir, os.Getuid(), "stopped-root.json", []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("se escribió en el destino del enlace: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(dir, "stopped-root.json"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("el archivo no es uno normal 0600: %v %v", fi, err)
	}
	if b, _ := readFileSafe(dir, os.Getuid(), "stopped-root.json", 100); string(b) != "[]" {
		t.Fatalf("contenido %q", b)
	}
	// No quedan temporales.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".stopped-root.json.") {
			t.Errorf("quedó el temporal %s", e.Name())
		}
	}
}

func TestSafeFileRejectsBadDirs(t *testing.T) {
	uid := os.Getuid()
	good := t.TempDir()
	_ = os.Chmod(good, 0o700)

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		dir   string
		owner int
	}{
		"la carpeta es un enlace":       {link, uid},
		"el grupo u otros escriben":     {open, uid},
		"es de otro usuario":            {good, uid + 1},
		"no existe":                     {filepath.Join(good, "nada"), uid},
		"es un archivo, no una carpeta": {writeTemp(t, good, "f"), uid},
	} {
		if err := writeFileSafe(tc.dir, tc.owner, "x", []byte("1"), 0o600); err == nil {
			t.Errorf("%s: writeFileSafe debió fallar", name)
		}
		if _, err := readFileSafe(tc.dir, tc.owner, "x", 10); err == nil {
			t.Errorf("%s: readFileSafe debió fallar", name)
		}
	}
	for _, bad := range []string{"", "..", "a/b", "../x"} {
		if err := writeFileSafe(good, uid, bad, nil, 0o600); err == nil {
			t.Errorf("nombre %q aceptado", bad)
		}
	}
}

func writeTemp(t *testing.T, dir, name string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Leer no sigue un enlace (p. ej. queue.json → /etc/shadow) ni abre algo que no sea
// la FIFO de la cola.
func TestReadAndFifoSafe(t *testing.T) {
	uid := os.Getuid()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	secret := writeTemp(t, t.TempDir(), "secret")
	if err := os.Symlink(secret, filepath.Join(dir, "queue.json")); err != nil {
		t.Fatal(err)
	}
	if b, err := readFileSafe(dir, uid, "queue.json", 100); err == nil {
		t.Fatalf("leyó a través del enlace: %q", b)
	}
	if _, err := readFileSafe(dir, uid, "nada", 100); !os.IsNotExist(err) {
		t.Errorf("archivo que falta: %v", err)
	}
	writeTemp(t, dir, "jobserver")
	if f, err := openFifoSafe(dir, uid, "jobserver"); err == nil {
		f.Close()
		t.Fatal("abrió un archivo normal como FIFO")
	}
	_ = os.Remove(filepath.Join(dir, "jobserver"))
	if err := syscall.Mkfifo(filepath.Join(dir, "jobserver"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openFifoSafe(dir, uid, "jobserver")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := removeFileSafe(dir, uid, "jobserver"); err != nil {
		t.Fatal(err)
	}
	// removeFileSafe borra el enlace, no su destino.
	if err := removeFileSafe(dir, uid, "queue.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("se borró el destino del enlace: %v", err)
	}
}

// La carpeta de root: se crea y se comprueba; una que otros pueden escribir no sirve.
func TestEnsureRootStateDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "turn_watch")
	oldBase, oldOwner, oldUID := rootStateBase, rootOwner, helperUID
	t.Cleanup(func() { rootStateBase, rootOwner, helperUID = oldBase, oldOwner, oldUID })
	rootStateBase, rootOwner, helperUID = base, os.Getuid(), 4242
	if err := ensureRootStateDir(); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "4242"); rootStateDir() != want {
		t.Fatalf("%s ≠ %s", rootStateDir(), want)
	}
	if err := writeFileSafe(rootStateDir(), rootOwner, helperPidName, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensureRootStateDir(); err == nil {
		t.Fatal("aceptó una carpeta base con escritura para todos")
	}
}

func TestEnforceTarget(t *testing.T) {
	uid := os.Getuid()
	parent := t.TempDir()
	dir := filepath.Join(parent, stateFolder)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := enforceTarget(dir, ""); err != nil || got != uid {
		t.Fatalf("válida: %d %v", got, err)
	}
	if got, err := enforceTarget(dir, strconv.Itoa(uid)); err != nil || got != uid {
		t.Fatalf("con SUDO_UID: %d %v", got, err)
	}
	if _, err := enforceTarget(dir, strconv.Itoa(uid+1)); err == nil {
		t.Error("aceptó una carpeta de otro usuario que el de sudo")
	}
	if _, err := enforceTarget("relativa/"+stateFolder, ""); err == nil {
		t.Error("aceptó una ruta relativa")
	}
	other := filepath.Join(parent, "otra")
	_ = os.Mkdir(other, 0o700)
	if _, err := enforceTarget(other, ""); err == nil {
		t.Error("aceptó una carpeta que no se llama turn_watch")
	}
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, stateFolder)
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := enforceTarget(link, ""); err == nil {
		t.Error("aceptó un enlace simbólico")
	}
	_ = os.Chmod(dir, 0o777)
	if _, err := enforceTarget(dir, ""); err == nil {
		t.Error("aceptó una carpeta con escritura para todos")
	}
}

func TestValidStopped(t *testing.T) {
	uid := 1000
	procs := map[int]*proc{
		10: {pid: 10, ppid: 1, comm: "cargo", start: 5},
		11: {pid: 11, ppid: 10, comm: "rustc", start: 50, paused: true, uid: uid},
		12: {pid: 12, ppid: 10, comm: "rustc", start: 60, uid: uid},                 // no está en pausa
		13: {pid: 13, ppid: 1, comm: "rustc", start: 70, paused: true, uid: uid},    // sin cargo
		14: {pid: 14, ppid: 10, comm: "sshd", start: 80, paused: true, uid: 0},      // no es un trabajo
		15: {pid: 15, ppid: 10, comm: "cc1", start: 90, paused: true, uid: uid + 1}, // otro usuario
	}
	u := func(n int) *int { return &n }
	for _, tc := range []struct {
		s  stoppedProc
		ok bool
	}{
		{stoppedProc{PID: 11, Start: 50, UID: u(uid)}, true},
		{stoppedProc{PID: 11, Start: 50}, true}, // archivo viejo, sin uid
		{stoppedProc{PID: 11, Start: 51}, false},
		{stoppedProc{PID: 11, Start: 0}, false},
		{stoppedProc{PID: 12, Start: 60}, false},
		{stoppedProc{PID: 13, Start: 70}, false},
		{stoppedProc{PID: 14, Start: 80}, false},
		{stoppedProc{PID: 15, Start: 90, UID: u(uid)}, false},
		{stoppedProc{PID: 99, Start: 1}, false},
		{stoppedProc{PID: 1, Start: 1}, false},
		{stoppedProc{PID: -1, Start: 1}, false},
	} {
		why := validStopped(tc.s, procs)
		if (why == "") != tc.ok {
			t.Errorf("%+v: %q (quería ok=%v)", tc.s, why, tc.ok)
		}
	}
}
