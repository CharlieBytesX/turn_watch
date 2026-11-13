//go:build linux

package main

// Migración desde rustwatch, la parte de systemd, cgroups y la barra de Omarchy
// (rustwatch solo existió en Linux). Ver migrate.go.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// stopOldService para y deshabilita rustwatch.service y borra su unidad. Espera a
// que el ayudante de root viejo termine (lo hace solo al desaparecer la cola vieja).
func stopOldService() (bool, string) {
	unit := filepath.Join(unitDir(), oldService)
	_, fileErr := os.Stat(unit)
	active := exec.Command("systemctl", "--user", "is-active", "--quiet", oldService).Run() == nil
	if fileErr != nil && !active {
		return false, ""
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", oldService).Run()
	_ = os.Remove(unit)
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()

	// El ayudante viejo (root) mira la cola vieja cada 200 ms y sale reanudando.
	note := ""
	pidFile := filepath.Join(oldRuntimeDir(), "helper.pid")
	if b, err := os.ReadFile(pidFile); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		gone := false
		for i := 0; i < 50 && pid > 0; i++ {
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
				gone = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if gone || pid <= 0 {
			note = "el ayudante de root viejo terminó"
		} else {
			note = fmt.Sprintf("el ayudante de root viejo (pid %d) sigue vivo", pid)
		}
	}
	// Lo que el servicio viejo dejó en pausa (sus propios procesos).
	resumeFile(filepath.Join(oldRuntimeDir(), "stopped.json"))
	return true, note
}

// cgroupPids: todos los procesos del cgroup y de sus hijos.
func cgroupPids(dir string) []int {
	var out []int
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			out = append(out, readPids(filepath.Join(p, "cgroup.procs"))...)
		}
		return nil
	})
	return out
}

func cgroupPopulated(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "populated 1")
}

// migrateSlice mueve todo lo que corre en rustwatch.slice (agentes y cargo) a la
// scope de agentes de turn_watch.slice, sin parar nada. Devuelve cuántos movió y
// cuántos quedaron en la vieja.
func migrateSlice() (moved, left int, err error) {
	c := newAgentCap()
	dir := filepath.Join(c.userCg, oldSlice)
	if _, e := os.Stat(dir); e != nil {
		return 0, 0, nil
	}
	if c.noBus {
		return 0, len(cgroupPids(dir)), fmt.Errorf("sin busctl")
	}
	// Varias pasadas: algo puede nacer en la vieja mientras se mueve (un hijo de
	// un proceso que todavía no se había movido).
	for pass := 0; pass < 5; pass++ {
		pids := cgroupPids(dir)
		if len(pids) == 0 {
			break
		}
		n, errs := c.moveTo(sliceName, agentScope, pids)
		moved += n
		for pid, e := range errs {
			if _, se := os.Stat(fmt.Sprintf("/proc/%d", pid)); se == nil {
				err = fmt.Errorf("pid %d: %v", pid, e)
			}
		}
		// Los que se fueron por su cuenta (o murieron) no cuentan como fallo.
		time.Sleep(100 * time.Millisecond)
	}
	// Sin cuota: lo que quede (no debería) corre sin el tope viejo. Sin `stop`.
	_ = exec.Command("systemctl", "--user", "set-property", "--runtime", oldSlice, "CPUQuota=").Run()
	left = len(cgroupPids(dir))
	if left == 0 && !cgroupPopulated(dir) {
		// Vacía: ya se puede borrar su archivo de unidad.
		_ = os.Remove(filepath.Join(unitDir(), oldSlice))
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	return moved, left, err
}

// migrateBar pone el plugin nuevo donde estaba el viejo y borra el viejo.
func migrateBar() string {
	oldDir := filepath.Join(home(), ".config", "omarchy", "plugins", oldName)
	msg := ""
	if onBar(oldName) {
		if renameOnBar(oldName, pluginID) {
			msg = "en el lugar del indicador viejo"
		}
	}
	if _, err := os.Stat(oldDir); err == nil {
		removeFromBar(oldName)
		_ = os.RemoveAll(oldDir)
		_ = exec.Command("omarchy-shell", "shell", "rescanPlugins").Run()
	}
	return msg
}

// renameOnBar cambia el id de un elemento de la barra sin moverlo de lugar.
func renameOnBar(from, to string) bool {
	cfg, layout := barLayout()
	if layout == nil {
		return false
	}
	changed := false
	for name, sec := range layout {
		items, _ := sec.([]any)
		kept := items[:0]
		for _, it := range items {
			m, ok := it.(map[string]any)
			switch {
			case ok && m["id"] == to && from != to:
				continue // ya estaba: queda solo el que ocupa el lugar del viejo
			case ok && m["id"] == from:
				m["id"] = to
				changed = true
			}
			kept = append(kept, it)
		}
		layout[name] = kept
	}
	if changed {
		writeShellConfig(cfg)
	}
	return changed
}

// cleanupOldSlice: si la instalación dejó procesos en rustwatch.slice, el servicio
// borra su archivo de unidad cuando se vacía (sin pararla nunca).
func cleanupOldSlice() {
	unit := filepath.Join(unitDir(), oldSlice)
	if _, err := os.Stat(unit); err != nil {
		return
	}
	dir := filepath.Join(newAgentCap().userCg, oldSlice)
	if _, err := os.Stat(dir); err == nil && cgroupPopulated(dir) {
		return
	}
	_ = os.Remove(unit)
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
}
