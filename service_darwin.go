//go:build darwin

package main

// macOS: el servicio es un LaunchAgent de launchd (~/Library/LaunchAgents) y los
// avisos van al Centro de notificaciones (osascript).
//
// No hay cgroups: el límite es la cola de núcleos (jobserver) y la pausa de los
// trabajos que sobran (SIGSTOP/SIGCONT), igual que en Linux, pero SIN tope de CPU
// (CPUQuota) ni para cargo ni para los agentes. Lo único extra es que el envoltorio
// de cargo, con el límite activo, baja la prioridad de cargo (nice 10): solo cede
// la CPU a lo demás cuando hay competencia; no la limita.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// fionread: _IOR('f', 127, int) — bytes esperando en la FIFO.
const fionread = 0x4004667f

const launchLabel = "io.github.charliebytesx.turn_watch"

const systemModeHint = "Para limitar también los procesos de root: `turn_watch sistema on` (pide sudo al activar).\n" +
	"Docker en macOS compila dentro de su máquina virtual: eso no se ve ni se limita."

// launchDomain: la sesión gráfica del usuario. Sin sesión gráfica (por ssh, en un
// servidor), el dominio de usuario.
func launchDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func userDomain() string { return fmt.Sprintf("user/%d", os.Getuid()) }

func plistPath() string {
	return filepath.Join(home(), "Library", "LaunchAgents", launchLabel+".plist")
}

func logPath() string { return filepath.Join(home(), "Library", "Logs", "turn_watch.log") }

// startHint: cómo arrancar el servicio a mano.
func startHint() string {
	return fmt.Sprintf("launchctl kickstart %s/%s (o turn_watch install)", launchDomain(), launchLabel)
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// launchPlist: el LaunchAgent. KeepAlive con SuccessfulExit=false: se reinicia si
// falla (como Restart=on-failure), no si se para a propósito.
func launchPlist(exe string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlText(exe) + `</string>
		<string>daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>EnvironmentVariables</key>
	<dict>
` + launchEnv() + `	</dict>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>Nice</key>
	<integer>10</integer>
	<key>StandardErrorPath</key>
	<string>` + xmlText(logPath()) + `</string>
	<key>StandardOutPath</key>
	<string>` + xmlText(logPath()) + `</string>
</dict>
</plist>
`
}

// launchEnv: el servicio ve el mismo HOME (y XDG_RUNTIME_DIR, si hay) que quien
// lo instala: así comparte la carpeta de estado y la configuración con el panel.
func launchEnv() string {
	var b strings.Builder
	for _, k := range []string{"HOME", "XDG_RUNTIME_DIR"} {
		if v := os.Getenv(k); v != "" {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", k, xmlText(v))
		}
	}
	return b.String()
}

// installService escribe el LaunchAgent y lo (re)carga.
func installService() {
	plist := plistPath()
	_ = os.MkdirAll(filepath.Dir(plist), 0o755)
	_ = os.MkdirAll(filepath.Dir(logPath()), 0o755)
	if err := os.WriteFile(plist, []byte(launchPlist(filepath.Join(binDir(), "turn_watch"))), 0o644); err != nil {
		warn("no se pudo escribir %s: %v", plist, err)
		return
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		warn("sin launchctl: no hay servicio; la cola no funciona sin él")
		return
	}
	// Recargar: bootout (si estaba) y bootstrap. bootout termina de forma asíncrona,
	// así que bootstrap se reintenta un poco.
	var out []byte
	var err error
	domain := ""
	for _, d := range []string{launchDomain(), userDomain()} {
		_ = exec.Command("launchctl", "bootout", d+"/"+launchLabel).Run()
		_ = exec.Command("launchctl", "enable", d+"/"+launchLabel).Run()
		for i := 0; i < 12; i++ {
			if out, err = exec.Command("launchctl", "bootstrap", d, plist).CombinedOutput(); err == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err == nil {
			domain = d
			break
		}
	}
	if domain == "" {
		warn("no se pudo cargar el servicio (launchctl bootstrap): %s", strings.TrimSpace(string(out)))
		return
	}
	step("servicio: launchd, %s (%s)", domain, plist)
	step("registro: %s", logPath())
	step("en macOS el límite es la cola y la pausa; no hay cgroups ni tope de CPU")
}

func installExtras() {}

func uninstallService() {
	for _, d := range []string{launchDomain(), userDomain()} {
		_ = exec.Command("launchctl", "bootout", d+"/"+launchLabel).Run()
	}
	if err := os.Remove(plistPath()); err == nil {
		step("servicio (launchd)")
	}
}

func uninstallExtras() {}

// setSliceQuota: sin cgroups no hay tope de CPU que poner.
func setSliceQuota(int) {}

// shimLimited: con la cola activa, baja la prioridad de cargo (y de lo que lance) a
// nice 10. Si ya era más baja (nice mayor), no la toca. Después sigue el envoltorio.
func shimLimited(args, env []string) {
	if cur, err := unix.Getpriority(unix.PRIO_PROCESS, 0); err == nil && cur < 10 {
		_ = unix.Setpriority(unix.PRIO_PROCESS, 0, 10)
	}
}

// cleanupOldSlice: rustwatch solo existió en Linux.
func cleanupOldSlice() {}

// ---------- agentes: sin tope ----------

// agentCapSupported: sin cgroups no hay tope de CPU para los agentes. Se ven igual
// (CPU, RAM, comandos), pero no se limitan.
const agentCapSupported = false

type agentCap struct{}

func newAgentCap() *agentCap        { return &agentCap{} }
func (c *agentCap) Capture([]*proc) {}
func (c *agentCap) Release()        {}
func (c *agentCap) Captured() int   { return 0 }

// runtimeBase: dónde va la carpeta de estado (cola, estado, pausados). $TMPDIR no
// sirve: el de launchd y el de la terminal pueden no ser el mismo. El ayudante de
// root recibe la carpeta con --dir.
func runtimeBase() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, "Library", "Caches")
	}
	return os.TempDir()
}
