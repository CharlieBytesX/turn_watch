//go:build linux

package main

// Tope de CPU para los agentes, sin alias: mientras el límite está activo, el servicio
// mueve a los agentes del usuario (y todos sus descendientes vivos) a
// turn_watch-agentes.scope, dentro de turn_watch.slice. Lo que lancen después nace ahí
// dentro y comparte el tope de la slice con las compilaciones.
//
// - Sin root: lo hace el systemd de usuario por D-Bus (busctl). Si la scope no existe
//   se crea con StartTransientUnit (Slice=turn_watch.slice, PIDs=[uno]); el resto se
//   mueve con AttachProcessesToUnit.
// - Lo que no se puede mover (otro usuario, un cgroup fuera de user@.service como una
//   sesión de ssh) se avisa una vez en el log y no se reintenta.
// - Al desactivar el límite se quedan donde están: la slice sin CPUQuota no limita.
// - Con agents_cap=false se sacan a turn_watch-agentes-libre.scope (en app.slice): si
//   no, seguirían con tope la próxima vez que se active el límite.
// - Nunca se pausa nada aquí: la pausa (SIGSTOP) es solo para trabajos de compilación.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// agentCapSupported: en Linux el tope de los agentes es real (cgroups de systemd).
const agentCapSupported = true

const (
	agentScope     = "turn_watch-agentes.scope"
	agentFreeScope = "turn_watch-agentes-libre.scope"
	agentFreeSlice = "app.slice"
)

type agentCap struct {
	failed  map[string]bool // pid-inicio que no se pudo mover
	badCg   map[string]bool // cgroups de los que no se puede sacar nada
	warned  map[string]bool
	noBus   bool
	userCg  string // /sys/fs/cgroup/…/user@<uid>.service
	lastErr string
}

func newAgentCap() *agentCap {
	c := &agentCap{failed: map[string]bool{}, badCg: map[string]bool{}, warned: map[string]bool{}}
	if _, err := exec.LookPath("busctl"); err != nil {
		c.noBus = true
	}
	// El servicio corre en user@<uid>.service/app.slice/turn_watch.service.
	if cg := cgroupOf(os.Getpid()); cg != "" {
		if i := strings.Index(cg, "/user@"); i >= 0 {
			if j := strings.Index(cg[i+1:], "/"); j >= 0 {
				c.userCg = "/sys/fs/cgroup" + cg[:i+1+j]
			} else {
				c.userCg = "/sys/fs/cgroup" + cg
			}
		}
	}
	if c.userCg == "" {
		uid := os.Getuid()
		c.userCg = fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service", uid, uid)
	}
	return c
}

func (c *agentCap) warn(key, format string, a ...any) {
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	fmt.Fprintf(os.Stderr, "turn_watch (agentes): "+format+"\n", a...)
}

// cgroupOf: la ruta del cgroup (v2) del proceso, o "" si ya no existe.
func cgroupOf(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "0::") {
			return strings.TrimPrefix(l, "0::")
		}
	}
	return ""
}

func (c *agentCap) scopeDir(slice, scope string) string {
	return filepath.Join(c.userCg, slice, scope)
}

// Captured: procesos dentro de la scope con tope ahora.
func (c *agentCap) Captured() int {
	return len(readPids(filepath.Join(c.scopeDir(sliceName, agentScope), "cgroup.procs")))
}

func readPids(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Capture mueve a la scope los procesos del árbol de agentes que no estén ya en
// turn_watch.slice. Solo mira los que le pasan (agentes y descendientes).
func (c *agentCap) Capture(tree []*proc) {
	if c.noBus {
		c.warn("busctl", "sin busctl: no se puede poner tope a los agentes")
		return
	}
	me := os.Getuid()
	var pids []int
	for _, p := range tree {
		if p.zombie {
			continue
		}
		key := fmt.Sprintf("%d-%d", p.pid, p.start)
		if c.failed[key] {
			continue
		}
		if p.uid != me {
			c.failed[key] = true
			c.warn("uid-"+strconv.Itoa(p.uid), "no se puede mover %d (%s): es de otro usuario (uid %d); sigue sin tope", p.pid, p.comm, p.uid)
			continue
		}
		cg := cgroupOf(p.pid)
		if cg == "" || strings.Contains(cg+"/", "/"+sliceName+"/") {
			continue
		}
		if c.badCg[cg] {
			c.failed[key] = true
			continue
		}
		if !strings.HasPrefix("/sys/fs/cgroup"+cg, c.userCg+"/") {
			// Fuera de user@.service (sesión de login, ssh…): systemd de usuario no
			// puede moverlo.
			c.badCg[cg] = true
			c.failed[key] = true
			c.warn("cg-"+cg, "no se puede mover %d (%s): su cgroup %s no es del systemd de usuario; sigue sin tope", p.pid, p.comm, cg)
			continue
		}
		pids = append(pids, p.pid)
	}
	if len(pids) == 0 {
		return
	}
	moved, errs := c.moveTo(sliceName, agentScope, pids)
	for pid, err := range errs {
		if _, e := os.Stat(fmt.Sprintf("/proc/%d", pid)); e != nil {
			continue // murió entre medio
		}
		c.failed[fmt.Sprintf("%d-%d", pid, startOf(pid))] = true
		c.warn("err-"+err.Error(), "no se pudo mover %d (y quizá otros: se avisa una vez) a %s: %v", pid, agentScope, err)
	}
	if moved > 0 {
		fmt.Fprintf(os.Stderr, "turn_watch (agentes): con tope en %s/%s: %d proceso(s) más\n", sliceName, agentScope, moved)
	}
	// Olvidar los fallos de procesos que ya no existen.
	if len(c.failed) > 4096 {
		c.failed = map[string]bool{}
	}
}

// Release saca de la scope con tope todo lo que haya (agents_cap=false).
func (c *agentCap) Release() {
	if c.noBus {
		return
	}
	pids := readPids(filepath.Join(c.scopeDir(sliceName, agentScope), "cgroup.procs"))
	if len(pids) == 0 {
		return
	}
	moved, errs := c.moveTo(agentFreeSlice, agentFreeScope, pids)
	for pid, err := range errs {
		c.warn("rel-"+err.Error(), "no se pudo sacar %d del tope: %v", pid, err)
	}
	if moved > 0 {
		fmt.Fprintf(os.Stderr, "turn_watch (agentes): sin tope (%s/%s): %d proceso(s)\n", agentFreeSlice, agentFreeScope, moved)
	}
}

// moveTo mete los pids en la scope (creándola en la slice si no existe). Devuelve
// cuántos quedaron dentro y los errores por pid.
func (c *agentCap) moveTo(slice, scope string, pids []int) (int, map[int]error) {
	errs := map[int]error{}
	dir := c.scopeDir(slice, scope)
	rest := pids
	if _, err := os.Stat(dir); err != nil {
		// No existe: se crea con el primer pid que siga vivo.
		created := false
		for i, pid := range pids {
			err := busctl("StartTransientUnit", "ssa(sv)a(sa(sv))", scope, "fail",
				"4", "Slice", "s", slice, "PIDs", "au", "1", strconv.Itoa(pid), "Delegate", "b", "true",
				"CollectMode", "s", "inactive-or-failed", "0")
			if err != nil {
				errs[pid] = err
				continue
			}
			rest = pids[i+1:]
			created = true
			break
		}
		if !created {
			return 0, errs
		}
		// El trabajo de systemd es asíncrono: esperar al cgroup.
		for i := 0; i < 50; i++ {
			if _, err := os.Stat(dir); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(rest) > 0 {
		if err := attach(scope, rest); err != nil {
			// Alguno falló (o murió): uno por uno. Una scope sin Delegate (creada por
			// una versión anterior) no acepta AttachProcessesToUnit; como el cgroup es
			// nuestro (dentro de user@.service), se escribe en su cgroup.procs.
			direct := strings.Contains(err.Error(), "non-delegated")
			for _, pid := range rest {
				e1 := err
				if !direct {
					if e1 = attach(scope, []int{pid}); e1 == nil {
						continue
					}
				}
				if e2 := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0); e2 != nil {
					errs[pid] = fmt.Errorf("%v; cgroup.procs: %v", e1, e2)
				}
			}
		}
	}
	in := map[int]bool{}
	for _, pid := range readPids(filepath.Join(dir, "cgroup.procs")) {
		in[pid] = true
	}
	n := 0
	for _, pid := range pids {
		if in[pid] {
			n++
			delete(errs, pid)
		}
	}
	return n, errs
}

func attach(scope string, pids []int) error {
	args := []string{"AttachProcessesToUnit", "ssau", scope, "", strconv.Itoa(len(pids))}
	for _, p := range pids {
		args = append(args, strconv.Itoa(p))
	}
	return busctl(args...)
}

// busctl llama a un método del Manager del systemd de usuario.
func busctl(args ...string) error {
	full := append([]string{"--user", "call", "--timeout=5", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager"}, args...)
	out, err := exec.Command("busctl", full...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
