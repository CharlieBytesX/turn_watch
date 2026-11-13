package main

// `turn_watch install` / `uninstall`: deja todo listo para el usuario actual con un solo
// ejecutable (se lo puedes pasar a otra persona tal cual).
//   - ~/.local/bin/turn_watch (este ejecutable) y ~/.local/bin/cargo → turn_watch
//     (el envoltorio que mete cada cargo en la cola mientras el TUI está abierto)
//   - el servicio de fondo: systemd de usuario en Linux (turn_watch.service y
//     turn_watch.slice, ver service_linux.go), launchd en macOS (service_darwin.go)
//   - en Omarchy: el indicador de la barra (plugin "turn_watch")

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		fail("sin directorio de inicio: %v", err)
	}
	return h
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "turn_watch: "+format+"\n", a...)
	os.Exit(1)
}

func step(format string, a ...any) { fmt.Printf("  ✓ "+format+"\n", a...) }
func warn(format string, a ...any) { fmt.Printf("  ! "+format+"\n", a...) }

func binDir() string { return filepath.Join(home(), ".local", "bin") }

func install() {
	fmt.Println("Instalando turn_watch para", os.Getenv("USER"))
	bin := binDir()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		fail("%v", err)
	}

	// 1. El ejecutable.
	dest := filepath.Join(bin, "turn_watch")
	self, _ := os.Executable()
	selfReal, _ := filepath.EvalSymlinks(self)
	destReal, _ := filepath.EvalSymlinks(dest)
	if selfReal != destReal {
		if err := copyFile(self, dest); err != nil {
			fail("copiando a %s: %v", dest, err)
		}
	}
	step("comando: %s", dest)

	// 2. El envoltorio de cargo (también reemplaza el de rustwatch).
	cargo := filepath.Join(bin, "cargo")
	if target, err := os.Readlink(cargo); err == nil && (filepath.Base(target) == "turn_watch" || filepath.Base(target) == oldName) {
		_ = os.Remove(cargo)
	} else if _, err := os.Lstat(cargo); err == nil {
		b, _ := os.ReadFile(cargo)
		if strings.Contains(string(b), "turn_watch") || strings.Contains(string(b), oldName) {
			_ = os.Remove(cargo) // envoltorio viejo
		} else {
			backup := cargo + ".antes-de-turn_watch"
			if err := os.Rename(cargo, backup); err != nil {
				fail("ya hay un %s y no se pudo apartar: %v", cargo, err)
			}
			warn("había otro %s: quedó en %s", cargo, backup)
		}
	}
	if err := os.Symlink("turn_watch", cargo); err != nil {
		fail("creando %s: %v", cargo, err)
	}
	step("envoltorio: %s → turn_watch", cargo)

	// 2b. Compatibilidad: `rustwatch …` (scripts con `rustwatch hold`) sigue andando.
	old := filepath.Join(bin, oldName)
	if target, err := os.Readlink(old); err != nil || filepath.Base(target) != "turn_watch" {
		if _, err := os.Lstat(old); err == nil {
			_ = os.Remove(old) // el ejecutable viejo (el servicio viejo sigue con su copia abierta)
			if err := os.Symlink("turn_watch", old); err == nil {
				step("compatibilidad: %s → turn_watch", old)
			}
		}
	}
	checkPath(bin)

	// 2c. Configuración de rustwatch.
	if migrateConfig() {
		step("configuración copiada de %s", oldConfigPath())
	}

	// 3. El servicio de fondo (systemd de usuario o launchd).
	installService()

	// 4. Caché de compilación (kache): se detecta o se instala, y se configura.
	if envVar("SIN_KACHE") == "" {
		setupKache()
	}

	// 5. Extras del escritorio (en Omarchy, el indicador de la barra).
	installExtras()

	cfg := loadConfig()
	saveConfig(cfg)

	// 6. Modo de todo el sistema: el ayudante de root, solo si sudo no pide
	// contraseña (la pide el panel, que tiene terminal).
	if cfg.System != nil && *cfg.System && cfg.Active {
		for i := 0; i < 30 && !queueActive(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		switch {
		case helperAlive():
			step("ayudante de root: ya corre")
		case exec.Command("sudo", "-n", "true").Run() == nil:
			ensureHelper()
			if helperAlive() {
				step("ayudante de root: turn_watch enforce (todo el sistema)")
			} else {
				warn("no arrancó el ayudante de root; abre `turn_watch` para reintentar")
			}
		default:
			warn("modo de todo el sistema: abre `turn_watch` para dar la contraseña de sudo")
		}
	}

	if cfg.Active {
		fmt.Printf("\nListo. Límite activo: %d de %d núcleos (`turn_watch` abre el panel).\n", cfg.Slots, numCPU())
	} else {
		fmt.Printf("\nListo. Abre `turn_watch` para activar el límite: tus compilaciones de Rust usarán\n"+
			"como mucho %d de %d núcleos y sigue activo aunque cierres el panel (d lo desactiva).\n", cfg.Slots, numCPU())
	}
	fmt.Println(systemModeHint)
}

func uninstall() {
	fmt.Println("Quitando turn_watch")
	uninstallService()
	cargo := filepath.Join(binDir(), "cargo")
	if target, err := os.Readlink(cargo); err == nil && filepath.Base(target) == "turn_watch" {
		_ = os.Remove(cargo)
		if _, err := os.Stat(cargo + ".antes-de-turn_watch"); err == nil {
			_ = os.Rename(cargo+".antes-de-turn_watch", cargo)
		}
		step("envoltorio de cargo")
	}
	old := filepath.Join(binDir(), oldName)
	if target, err := os.Readlink(old); err == nil && filepath.Base(target) == "turn_watch" {
		_ = os.Remove(old)
		step("enlace %s", old)
	}
	uninstallExtras()
	fmt.Printf("Queda %s y tu configuración en %s; bórralos a mano si quieres.\n",
		filepath.Join(binDir(), "turn_watch"), filepath.Dir(configPath()))
}

// checkPath avisa si el cargo de verdad va antes que el envoltorio en el PATH.
func checkPath(bin string) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == bin {
			return
		}
		if fi, err := os.Stat(filepath.Join(dir, "cargo")); err == nil && !fi.IsDir() {
			warn("en tu PATH, %s va antes que %s: el envoltorio no se usará.", dir, bin)
			warn("pon %s primero (p. ej. en ~/.profile o config.fish)", bin)
			return
		}
	}
	warn("%s no está en tu PATH: agrégalo antes de ~/.cargo/bin", bin)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
