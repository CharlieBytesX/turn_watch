package main

// Dónde escribe el ayudante de root.
//
// Antes, `sudo turn_watch enforce` escribía stopped-root.json y helper.pid en la
// carpeta del usuario ($XDG_RUNTIME_DIR/turn_watch), que el usuario controla: un
// enlace simbólico ahí bastaba para que root sobrescribiera cualquier archivo, y
// root reanudaba los pids que el usuario quisiera escribir en esa lista. Ahora:
//
//   - lo que escribe root va a rootStateBase/<uid> (/run/turn_watch/<uid> en Linux,
//     /var/run/turn_watch/<uid> en macOS): carpetas de root, 0755 (el servicio del
//     usuario tiene que poder leer helper.pid), que se comprueban al abrirlas;
//   - todo se escribe con writeFileSafe (O_NOFOLLOW|O_EXCL sobre la carpeta ya
//     comprobada) y se lee con readFileSafe;
//   - antes de reanudar un pid de una lista guardada se comprueba que sea lo que
//     turn_watch pausó: el mismo proceso (inicio), en pausa, un trabajo de
//     compilación bajo cargo y, si se anotó, del mismo usuario (validStopped).

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// rootOwner: el dueño de las carpetas de root (una variable solo para las pruebas).
var rootOwner = 0

// rootStateBase: la carpeta de root para el estado de los ayudantes (ver
// defaultRootStateBase en service_*.go). Variable para las pruebas.
var rootStateBase = defaultRootStateBase

// helperUID: el usuario cuyo límite aplica el ayudante de root. En el servicio y el
// panel es el propio; en `sudo turn_watch enforce`, el dueño de --dir.
var helperUID = os.Getuid()

func rootStateDir() string { return filepath.Join(rootStateBase, strconv.Itoa(helperUID)) }

// ensureRootStateDir crea (como root) rootStateBase y rootStateBase/<uid> y comprueba
// que sean carpetas de root sin escritura para nadie más.
func ensureRootStateDir() error {
	if err := ensureOwnedDir(rootStateBase, rootOwner, 0o755); err != nil {
		return err
	}
	return ensureOwnedDir(rootStateDir(), rootOwner, 0o755)
}

// enforceTarget valida el --dir de `sudo turn_watch enforce`: la carpeta de estado
// de un usuario ($XDG_RUNTIME_DIR/turn_watch), de verdad una carpeta (no un enlace),
// suya, sin escritura para otros y, si se llegó por sudo, del usuario que llamó a
// sudo. Devuelve su dueño.
func enforceTarget(dir string, sudoUID string) (int, error) {
	if !filepath.IsAbs(dir) || filepath.Base(dir) != stateFolder {
		return 0, fmt.Errorf("--dir debe ser una ruta absoluta que termine en /%s: %q", stateFolder, dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return 0, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return 0, fmt.Errorf("%s no es una carpeta (¿un enlace?)", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: sin dueño", dir)
	}
	owner := int(st.Uid)
	if sudoUID != "" {
		want, err := strconv.Atoi(strings.TrimSpace(sudoUID))
		if err != nil {
			return 0, fmt.Errorf("SUDO_UID no válido: %q", sudoUID)
		}
		if want != owner {
			return 0, fmt.Errorf("%s es del usuario %d, pero sudo lo pidió el %d", dir, owner, want)
		}
	}
	if err := checkDir(dir, owner); err != nil {
		return 0, err
	}
	return owner, nil
}

// validStopped: ¿es s el proceso que turn_watch pausó? Si no, no se toca. procs es
// una lectura de la tabla de procesos de ahora. "" = sí; si no, el motivo.
//
// No se exige un cgroup concreto: la pausa alcanza a propósito a compilaciones que
// no pasan por el envoltorio ni por turn_watch.slice (Docker, otro cargo).
func validStopped(s stoppedProc, procs map[int]*proc) string {
	p := procs[s.PID]
	switch {
	case s.PID <= 1:
		return "pid no válido"
	case p == nil:
		return "ya no existe"
	case s.Start == 0 || p.start != s.Start:
		return "es otro proceso (pid reciclado)"
	case s.UID != nil && p.uid != *s.UID:
		return fmt.Sprintf("es del usuario %d, no del %d", p.uid, *s.UID)
	case !isJob(p):
		return fmt.Sprintf("%q no es un trabajo de compilación", p.comm)
	case !underCargo(procs, p):
		return "no cuelga de un cargo"
	case !p.paused:
		return "no está en pausa"
	}
	return ""
}
