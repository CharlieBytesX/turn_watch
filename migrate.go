package main

// Migración desde rustwatch (el nombre anterior). La hace `turn_watch install` y es
// segura de repetir: lo que ya no está, se salta.
//
//   - config: ~/.config/rustwatch/config.json → ~/.config/turn_watch/config.json
//     (solo si la nueva no existe);
//   - rustwatch.service: se para y deshabilita (al pararse suelta su cola y su
//     cuota, y el ayudante de root viejo termina solo reanudando lo que pausó);
//   - rustwatch.slice: NUNCA se para (pararla mata lo que tiene dentro, que son los
//     agentes, este mismo incluido). Sus procesos se MUEVEN a
//     turn_watch.slice/turn_watch-agentes.scope; luego se le quita la cuota y, si
//     quedó vacía, se borra su archivo de unidad. Vacía, systemd la recoge solo;
//   - la barra: el plugin nuevo va en el lugar del viejo.

import (
	"os"
	"path/filepath"
)

const (
	oldName    = "rustwatch"
	oldService = "rustwatch.service"
	oldSlice   = "rustwatch.slice"
)

func oldRuntimeDir() string { return filepath.Join(filepath.Dir(runtimeDir()), oldName) }

func oldConfigPath() string {
	return filepath.Join(filepath.Dir(filepath.Dir(configPath())), oldName, "config.json")
}

// migrateConfig copia la configuración vieja si la nueva no existe.
func migrateConfig() bool {
	if _, err := os.Stat(configPath()); err == nil {
		return false
	}
	b, err := os.ReadFile(oldConfigPath())
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return false
	}
	return os.WriteFile(configPath(), b, 0o644) == nil
}
