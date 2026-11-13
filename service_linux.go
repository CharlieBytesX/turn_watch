//go:build linux

package main

// Linux: el servicio y el tope de CPU van con el systemd de usuario
// (turn_watch.service y turn_watch.slice); en Omarchy, además, el indicador de la
// barra.

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// fionread: ioctl para saber cuántos bytes esperan en la FIFO.
const fionread = unix.TIOCINQ

const sliceName = "turn_watch.slice"

// systemModeHint: qué alcanza el modo de todo el sistema.
const systemModeHint = "Para limitar también root y Docker: `turn_watch sistema on` (pide sudo al activar)."

// startHint: cómo arrancar el servicio a mano.
func startHint() string { return "systemctl --user start turn_watch.service" }

//go:embed omarchy/manifest.json omarchy/BarWidget.qml
var omarchyPlugin embed.FS

const pluginID = "turn_watch"

const serviceUnit = `[Unit]
Description=turn_watch: compilaciones de Rust y agentes (límite, avisos y barra)
After=graphical-session.target
PartOf=graphical-session.target

[Service]
ExecStart=%h/.local/bin/turn_watch daemon
Restart=on-failure
RestartSec=5
Nice=10

[Install]
WantedBy=graphical-session.target
`

const sliceUnit = `[Unit]
Description=turn_watch: compilaciones y agentes con límite de CPU

[Slice]
# El tope (CPUQuota) lo pone y lo quita turn_watch en caliente.
CPUWeight=50
`

func unitDir() string     { return filepath.Join(home(), ".config", "systemd", "user") }
func pluginDir() string   { return filepath.Join(home(), ".config", "omarchy", "plugins", pluginID) }
func shellConfig() string { return filepath.Join(home(), ".config", "omarchy", "shell.json") }

// installService escribe y arranca turn_watch.service y turn_watch.slice, y migra
// lo que quede de rustwatch.
func installService() {
	if _, err := exec.LookPath("systemctl"); err == nil && userSystemd() {
		_ = os.MkdirAll(unitDir(), 0o755)
		must(os.WriteFile(filepath.Join(unitDir(), "turn_watch.service"), []byte(serviceUnit), 0o644))
		must(os.WriteFile(filepath.Join(unitDir(), sliceName), []byte(sliceUnit), 0o644))
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()

		// Primero el servicio viejo (si no, volvería a meter los agentes en su slice).
		if was, note := stopOldService(); was {
			step("rustwatch.service parado, deshabilitado y borrado")
			if note != "" {
				step("%s", note)
			}
		}
		moved, left, err := migrateSlice()
		switch {
		case err != nil:
			warn("moviendo procesos de %s: %v (quedan %d)", oldSlice, err, left)
		case left > 0:
			warn("quedan %d procesos en %s (no se para; sin cuota)", left, oldSlice)
		case moved > 0:
			step("%d procesos movidos de %s a %s/%s, sin pararlos", moved, oldSlice, sliceName, agentScope)
		}

		if out, err := exec.Command("systemctl", "--user", "enable", "--now", "turn_watch.service").CombinedOutput(); err != nil {
			warn("no se pudo arrancar turn_watch.service: %s", strings.TrimSpace(string(out)))
		} else {
			_ = exec.Command("systemctl", "--user", "restart", "turn_watch.service").Run()
			step("servicio: turn_watch.service")
		}
		step("límite de CPU: %s (lo aplica el servicio mientras el límite esté activo)", sliceName)
	} else {
		warn("sin systemd de usuario: no hay avisos ni tope de CPU; la cola funciona igual")
	}
}

// installExtras: el indicador de la barra en Omarchy.
func installExtras() {
	if _, err := exec.LookPath("omarchy"); err == nil {
		if err := installOmarchyPlugin(); err != nil {
			warn("indicador de la barra: %v", err)
		} else {
			step("indicador en la barra de Omarchy (clic: abre turn_watch)")
		}
	}
}

func uninstallService() {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.Command("systemctl", "--user", "disable", "--now", "turn_watch.service").Run()
		_ = os.Remove(filepath.Join(unitDir(), "turn_watch.service"))
		// La slice no se para: si hay agentes dentro, los mataría. Sin cuota no limita.
		setSliceQuota(0)
		_ = os.Remove(filepath.Join(unitDir(), sliceName))
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		step("servicio y slice")
	}
}

func uninstallExtras() {
	if _, err := os.Stat(pluginDir()); err == nil {
		removeFromBar(pluginID)
		_ = os.RemoveAll(pluginDir())
		step("indicador de la barra")
	}
}

// setSliceQuota pone (o con 0 quita) el tope de CPU de turn_watch.slice. Sin systemd
// de usuario no hace nada: queda solo la cola.
func setSliceQuota(slots int) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	quota := ""
	if slots > 0 {
		quota = fmt.Sprintf("%d%%", slots*100)
	}
	_ = exec.Command("systemctl", "--user", "set-property", "--runtime", sliceName, "CPUQuota="+quota).Run()
}

func userSystemd() bool {
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

// shimLimited: con la cola activa, el envoltorio de cargo corre cargo dentro de
// turn_watch.slice (systemd-run --scope), que tiene el tope de CPU. Si no hay
// systemd de usuario, vuelve y cargo corre solo con la cola.
func shimLimited(args, env []string) {
	if sr, err := exec.LookPath("systemd-run"); err == nil && userSystemd() {
		args = append([]string{sr, "--user", "--scope", "--slice=" + sliceName, "--quiet", "--collect", "--"}, args...)
		must(syscall.Exec(sr, args, env))
	}
}

func installOmarchyPlugin() error {
	if err := os.MkdirAll(pluginDir(), 0o755); err != nil {
		return err
	}
	for _, name := range []string{"manifest.json", "BarWidget.qml"} {
		b, err := omarchyPlugin.ReadFile("omarchy/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pluginDir(), name), b, 0o644); err != nil {
			return err
		}
	}
	_ = exec.Command("omarchy-shell", "shell", "rescanPlugins").Run()
	// Donde estaba el de rustwatch, si estaba; si no, donde lo ponga omarchy.
	if msg := migrateBar(); msg != "" {
		step("barra: plugin %s %s", pluginID, msg)
	}
	if !onBar(pluginID) {
		if out, err := exec.Command("omarchy", "bar", "put", pluginID).CombinedOutput(); err != nil {
			return fmt.Errorf("omarchy bar put: %s", strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// onBar / removeFromBar: el layout de la barra en ~/.config/omarchy/shell.json.
func barLayout() (map[string]any, map[string]any) {
	b, err := os.ReadFile(shellConfig())
	if err != nil {
		return nil, nil
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return nil, nil
	}
	bar, _ := cfg["bar"].(map[string]any)
	layout, _ := bar["layout"].(map[string]any)
	return cfg, layout
}

func onBar(id string) bool {
	_, layout := barLayout()
	for _, sec := range layout {
		items, _ := sec.([]any)
		for _, it := range items {
			if m, ok := it.(map[string]any); ok && m["id"] == id {
				return true
			}
		}
	}
	return false
}

func removeFromBar(id string) {
	cfg, layout := barLayout()
	if layout == nil {
		return
	}
	changed := false
	for name, sec := range layout {
		items, _ := sec.([]any)
		kept := items[:0]
		for _, it := range items {
			if m, ok := it.(map[string]any); ok && m["id"] == id {
				changed = true
				continue
			}
			kept = append(kept, it)
		}
		layout[name] = kept
	}
	if !changed {
		return
	}
	writeShellConfig(cfg)
}

// writeShellConfig guarda shell.json en el mismo archivo (la barra lo vigila).
func writeShellConfig(cfg map[string]any) {
	if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		_ = os.WriteFile(shellConfig(), append(b, '\n'), 0o644)
	}
}

// runtimeBase: dónde va la carpeta de estado si no hay XDG_RUNTIME_DIR.
func runtimeBase() string { return os.TempDir() }
