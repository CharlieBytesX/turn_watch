package main

// Límite a la fuerza: con el TUI abierto, ningún momento hay más de N trabajos de
// compilación corriendo, vengan de donde vengan (cargo con o sin el envoltorio, el
// editor, root, Docker). Los que sobran se pausan (SIGSTOP) y se reanudan (SIGCONT)
// cuando hay turno: siguen los más antiguos, esperan los nuevos. Cambiar el límite
// actúa al instante, también sobre compilaciones ya en curso.
//
// - Sin sudo, el TUI lo hace con los procesos del usuario.
// - Con sudo, `turn_watch enforce` corre como root mientras el TUI esté abierto y
//   alcanza también a root, Docker (BuildKit) y otros usuarios.
// - Nunca deja nada pausado: al cerrar reanuda todo, y si el TUI muere de golpe el
//   ayudante de root (o el daemon) reanuda lo que quedó en stopped.json.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// isJob: procesos que cuentan como un trabajo de compilación.
func isJob(p *proc) bool {
	switch p.comm {
	case "rustc", "cc1", "cc1plus":
		return true
	}
	return strings.HasPrefix(p.comm, "build-script")
}

type stoppedProc struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
	// UID: dueño del proceso al pausarlo (nil en archivos de versiones anteriores).
	UID *int `json:"uid,omitempty"`
}

// Enforcer pausa y reanuda trabajos para no pasar del límite.
type Enforcer struct {
	dir     string // carpeta del archivo de pausados (comprobada al escribir)
	owner   int    // dueño esperado de esa carpeta
	name    string
	stopped map[int]stoppedProc // pid → lo que se pausó (inicio: para no confundir pids reciclados)
	Paused  int                 // pausados ahora (para la vista)
	Denied  int                 // trabajos que no se pueden tocar (de otro usuario, sin root)
}

// stoppedLocation: dónde guarda sus pausados el servicio del usuario (su carpeta) o
// el ayudante de root (la carpeta de root, nunca la del usuario).
func stoppedLocation(root bool) (dir string, owner int, name string) {
	if root {
		return rootStateDir(), rootOwner, "stopped-root.json"
	}
	return runtimeDir(), os.Getuid(), "stopped.json"
}

func newEnforcer(root bool) *Enforcer {
	dir, owner, name := stoppedLocation(root)
	e := &Enforcer{dir: dir, owner: owner, name: name, stopped: map[int]stoppedProc{}}
	resumeFile(dir, owner, name) // de una sesión que murió sin reanudar
	return e
}

// resumeFile reanuda los pausados que dejó otra sesión en dir/name, pero solo los
// que siguen siendo lo que se pausó (validStopped), y borra el archivo. La carpeta
// debe ser de owner y el archivo se lee sin seguir enlaces.
func resumeFile(dir string, owner int, name string) {
	b, err := readFileSafe(dir, owner, name, 1<<20)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "turn_watch: no se leen los pausados:", err)
		}
		return
	}
	var list []stoppedProc
	_ = json.Unmarshal(b, &list)
	if len(list) > 0 {
		procs := readProcsLite()
		for _, s := range list {
			if why := validStopped(s, procs); why != "" {
				fmt.Fprintf(os.Stderr, "turn_watch: no se reanuda el pid %d: %s\n", s.PID, why)
				continue
			}
			_ = syscall.Kill(s.PID, syscall.SIGCONT)
		}
	}
	_ = removeFileSafe(dir, owner, name)
}

func (e *Enforcer) save() {
	if len(e.stopped) == 0 {
		_ = removeFileSafe(e.dir, e.owner, e.name)
		return
	}
	list := make([]stoppedProc, 0, len(e.stopped))
	for _, s := range e.stopped {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].PID < list[j].PID })
	b, _ := json.Marshal(list)
	if err := writeFileSafe(e.dir, e.owner, e.name, b, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "turn_watch: no se guardan los pausados:", err)
	}
}

// Tick deja corriendo como mucho `limit` trabajos (los más antiguos).
func (e *Enforcer) Tick(limit int) {
	procs := readProcsLite()
	var jobs []*proc
	for _, p := range procs {
		if !isJob(p) || !underCargo(procs, p) {
			continue
		}
		jobs = append(jobs, p)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].start != jobs[j].start {
			return jobs[i].start < jobs[j].start
		}
		return jobs[i].pid < jobs[j].pid
	})
	alive := map[int]bool{}
	running := 0
	e.Denied = 0
	for _, p := range jobs {
		alive[p.pid] = true
		_, mine := e.stopped[p.pid]
		if running < limit {
			if mine {
				_ = syscall.Kill(p.pid, syscall.SIGCONT)
				delete(e.stopped, p.pid)
			}
			running++
			continue
		}
		if mine {
			continue
		}
		if err := syscall.Kill(p.pid, syscall.SIGSTOP); err != nil {
			// De otro usuario y sin root: corre igual (cuenta, pero no se puede pausar).
			e.Denied++
			running++
			continue
		}
		uid := p.uid
		e.stopped[p.pid] = stoppedProc{PID: p.pid, Start: p.start, UID: &uid}
	}
	for pid := range e.stopped {
		if !alive[pid] {
			delete(e.stopped, pid)
		}
	}
	e.Paused = len(e.stopped)
	e.save()
}

// Release reanuda todo lo que pausó.
func (e *Enforcer) Release() {
	for pid, s := range e.stopped {
		if startOf(pid) == s.Start {
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	}
	e.stopped = map[int]stoppedProc{}
	_ = removeFileSafe(e.dir, e.owner, e.name)
}

// underCargo: el trabajo cuelga de un cargo (local o dentro de Docker).
func underCargo(procs map[int]*proc, p *proc) bool {
	for q := procs[p.ppid]; q != nil && q.pid > 1; q = procs[q.ppid] {
		if isCargo(q) {
			return true
		}
	}
	return false
}

// ---------- ayudante de root ----------

// enforceCmd: `sudo turn_watch enforce --dir DIR`. Corre como root mientras el límite
// del usuario esté activo (lo mantiene su servicio) y aplica el límite que publica.
func enforceCmd(args []string) {
	var dir string
	detach := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case "--detach":
			detach = true
		}
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "uso: sudo turn_watch enforce --dir DIR")
		os.Exit(2)
	}
	if os.Geteuid() != 0 {
		fail("enforce: es el ayudante de root; se lanza con sudo")
	}
	dir = filepath.Clean(dir)
	uid, err := enforceTarget(dir, os.Getenv("SUDO_UID"))
	if err != nil {
		fail("enforce: %v", err)
	}
	helperUID = uid
	_ = os.Setenv("XDG_RUNTIME_DIR", filepath.Dir(dir))
	if err := ensureRootStateDir(); err != nil {
		fail("enforce: %v", err)
	}
	if helperAlive() {
		return // ya hay uno
	}
	if detach {
		// Ya como root: seguir en una sesión propia para sobrevivir al panel y a su
		// terminal, y devolverle el control a sudo enseguida.
		exe, _ := os.Executable()
		c := exec.Command(exe, "enforce", "--dir", dir)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "turn_watch enforce:", err)
			os.Exit(1)
		}
		return
	}
	signal.Ignore(syscall.SIGHUP)
	if err := writeFileSafe(rootStateDir(), rootOwner, helperPidName, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fail("enforce: %v", err)
	}
	defer removeFileSafe(rootStateDir(), rootOwner, helperPidName)
	e := newEnforcer(true)
	// Lo que dejaron en pausa ayudantes de versiones anteriores, que escribían en la
	// carpeta del usuario: se lee sin seguir enlaces y cada pid se valida.
	resumeFile(dir, uid, "stopped-root.json")
	resumeFile(filepath.Join(filepath.Dir(dir), oldName), uid, "stopped-root.json")
	defer e.Release()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		slots, _ := queueStatus()
		if slots == 0 {
			return // límite desactivado o servicio detenido
		}
		e.Tick(slots)
		select {
		case <-sig:
			return
		case <-t.C:
		}
	}
}

// ---------- arranque desde el TUI ----------

// wantSystem decide si se limita todo el sistema (con sudo) o solo el usuario. Pregunta
// antes de abrir el TUI; la respuesta queda en la configuración.
func wantSystem() bool {
	if os.Geteuid() == 0 {
		return false // el TUI ya es root: lo hace él mismo
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return false
	}
	cfg := loadConfig()
	if cfg.System != nil && !*cfg.System {
		return false
	}
	if exec.Command("sudo", "-n", "true").Run() == nil {
		return true
	}
	if cfg.System == nil {
		fmt.Print("turn_watch puede limitar también las compilaciones de root, Docker y otros\n" +
			"usuarios (pausa las que sobran). Para eso necesita sudo. ¿Activarlo? [S/n] ")
		var ans string
		_, _ = fmt.Scanln(&ans)
		if a := strings.ToLower(strings.TrimSpace(ans)); a == "n" || a == "no" {
			setSystemConfig(false)
			fmt.Println("Solo tu usuario. Para cambiarlo: turn_watch sistema on")
			return false
		}
		setSystemConfig(true)
	}
	fmt.Println("turn_watch: contraseña de sudo para limitar todo el sistema:")
	c := exec.Command("sudo", "-v")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if c.Run() != nil {
		fmt.Println("Sin sudo: se limita solo tu usuario.")
		time.Sleep(time.Second)
		return false
	}
	return true
}

// startRootHelper lanza `sudo turn_watch enforce` (si no corre ya). nil: sin root.
func startRootHelper() *exec.Cmd {
	if helperAlive() {
		return nil
	}
	if !wantSystem() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	// Desde esta terminal (el permiso de sudo es por terminal); el ayudante se
	// separa solo (--detach) y sigue aunque se cierre el panel.
	c := exec.Command("sudo", "-n", exe, "enforce", "--dir", runtimeDir(), "--detach")
	if c.Start() != nil {
		return nil
	}
	return c
}

// sistemaCmd: `turn_watch sistema on|off`.
func sistemaCmd(args []string) {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		cfg := loadConfig()
		state := "preguntar al abrir el TUI"
		if cfg.System != nil {
			state = map[bool]string{true: "on (todo el sistema, con sudo)", false: "off (solo tu usuario)"}[*cfg.System]
		}
		fmt.Println("sistema:", state, "· cambiar: turn_watch sistema on|off")
		return
	}
	setSystemConfig(args[0] == "on")
	fmt.Println("sistema:", args[0], "(se aplica al abrir el TUI)")
}
