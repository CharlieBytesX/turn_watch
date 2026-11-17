package main

// Lo que turn_watch necesita saber de la configuración de cargo, leída como la lee
// cargo (TOML de verdad, no buscando una línea):
//
//   - qué rustc-wrapper usa cargo: RUSTC_WRAPPER, luego CARGO_BUILD_RUSTC_WRAPPER y
//     luego build.rustc-wrapper en $CARGO_HOME/config.toml (o config). Si ya hay otro
//     (sccache, una ruta propia…), `turn_watch install` no lo pisa con `kache init`.
//     Una variable vacía (RUSTC_WRAPPER=) apaga el wrapper, como en cargo.
//   - build.target-dir, para contar el progreso de una compilación (progress.go).
//   - si el cargo que encuentra la shell es el envoltorio o se lo salta (otro cargo
//     antes en el PATH, como los shims de mise o asdf).
//
// No se miran los .cargo/config.toml de cada proyecto para el wrapper: install y el
// servicio no saben desde qué proyecto se compilará.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// WrapperKind: qué rustc-wrapper usa cargo.
type WrapperKind string

const (
	WrapperNone    WrapperKind = "none"    // ninguno
	WrapperKache   WrapperKind = "kache"   // kache
	WrapperOther   WrapperKind = "other"   // otro (sccache, uno propio…)
	WrapperOff     WrapperKind = "off"     // apagado a propósito (variable vacía)
	WrapperUnknown WrapperKind = "unknown" // la configuración no se pudo leer
)

// WrapperInfo: el rustc-wrapper en efecto y de dónde sale.
type WrapperInfo struct {
	Kind   WrapperKind `json:"kind"`
	Value  string      `json:"value,omitempty"`
	Source string      `json:"source,omitempty"` // "RUSTC_WRAPPER", "/home/x/.cargo/config.toml"…
	Err    string      `json:"error,omitempty"`
}

func (w WrapperInfo) String() string {
	switch w.Kind {
	case WrapperNone:
		return "cargo no usa ningún rustc-wrapper"
	case WrapperKache:
		return fmt.Sprintf("rustc-wrapper = %q (%s)", w.Value, w.Source)
	case WrapperOther:
		return fmt.Sprintf("cargo ya usa otro rustc-wrapper: %q (%s)", w.Value, w.Source)
	case WrapperOff:
		return fmt.Sprintf("rustc-wrapper apagado: %s vacío", w.Source)
	case WrapperUnknown:
		return fmt.Sprintf("no se pudo leer la configuración de cargo: %s", w.Err)
	}
	return string(w.Kind)
}

func classifyWrapper(value, source string) WrapperInfo {
	v := strings.TrimSpace(value)
	if v == "" {
		return WrapperInfo{Kind: WrapperOff, Source: source}
	}
	base := strings.TrimSuffix(filepath.Base(v), ".exe")
	if base == "kache" {
		return WrapperInfo{Kind: WrapperKache, Value: v, Source: source}
	}
	return WrapperInfo{Kind: WrapperOther, Value: v, Source: source}
}

// cargoHomeDir: $CARGO_HOME (relativo: al directorio actual) o ~/.cargo.
func cargoHomeDir(lookup func(string) (string, bool), home string) string {
	if v, ok := lookup("CARGO_HOME"); ok && v != "" {
		if filepath.IsAbs(v) {
			return v
		}
		if wd, err := os.Getwd(); err == nil {
			return filepath.Join(wd, v)
		}
		return v
	}
	return filepath.Join(home, ".cargo")
}

// cargoConfigIn: el archivo de configuración de una carpeta .cargo (o CARGO_HOME):
// "config" si existe (cargo lo prefiere si están los dos), si no "config.toml".
func cargoConfigIn(dir string) string {
	for _, name := range []string{"config", "config.toml"} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// cargoBuildTable lee la tabla [build] de un config de cargo.
func cargoBuildTable(path string) (map[string]any, error) {
	var doc map[string]any
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return nil, err
	}
	switch b := doc["build"].(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return b, nil
	default:
		return nil, fmt.Errorf("%s: build no es una tabla", path)
	}
}

// detectWrapper: el rustc-wrapper que usaría cargo con este entorno.
func detectWrapper(lookup func(string) (string, bool), home string) WrapperInfo {
	for _, key := range []string{"RUSTC_WRAPPER", "CARGO_BUILD_RUSTC_WRAPPER"} {
		if v, ok := lookup(key); ok {
			return classifyWrapper(v, key)
		}
	}
	path := cargoConfigIn(cargoHomeDir(lookup, home))
	if path == "" {
		return WrapperInfo{Kind: WrapperNone}
	}
	build, err := cargoBuildTable(path)
	if err != nil {
		return WrapperInfo{Kind: WrapperUnknown, Source: path, Err: err.Error()}
	}
	switch v := build["rustc-wrapper"].(type) {
	case nil:
		return WrapperInfo{Kind: WrapperNone, Source: path}
	case string:
		if strings.TrimSpace(v) == "" {
			return WrapperInfo{Kind: WrapperOff, Source: path}
		}
		return classifyWrapper(v, path)
	default:
		return WrapperInfo{Kind: WrapperUnknown, Source: path, Err: fmt.Sprintf("build.rustc-wrapper no es texto (%T)", v)}
	}
}

// configTargetDir: build.target-dir según los .cargo/config(.toml) desde cwd hacia
// arriba y luego CARGO_HOME, como cargo (el primero que lo diga gana). Una ruta
// relativa es relativa a la carpeta que contiene .cargo.
func configTargetDir(cwd, cargoHome string) string {
	var dirs []string
	for d := cwd; d != "" && d != "/" && d != "."; d = filepath.Dir(d) {
		dirs = append(dirs, filepath.Join(d, ".cargo"))
		if filepath.Dir(d) == d {
			break
		}
	}
	dirs = append(dirs, cargoHome)
	for _, d := range dirs {
		path := cargoConfigIn(d)
		if path == "" {
			continue
		}
		build, err := cargoBuildTable(path)
		if err != nil {
			continue
		}
		if v, ok := build["target-dir"].(string); ok && v != "" {
			if filepath.IsAbs(v) {
				return v
			}
			return filepath.Join(filepath.Dir(d), v)
		}
	}
	return ""
}

// cargoBypass: el primer `cargo` del PATH. bypass=true si no es el envoltorio de
// turn_watch (en bin, o un enlace a self): esas compilaciones no pasan por la cola
// ni por turn_watch.slice. first="" si no hay ningún cargo en el PATH.
func cargoBypass(path, bin, self string) (first string, bypass bool) {
	selfReal, _ := filepath.EvalSymlinks(self)
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, "cargo")
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if filepath.Clean(dir) == filepath.Clean(bin) {
			return cand, false
		}
		if r, err := filepath.EvalSymlinks(cand); err == nil && selfReal != "" && r == selfReal {
			return cand, false
		}
		return cand, true
	}
	return "", false
}

// localWarnings: avisos que dependen del entorno de quien mira (su PATH y sus
// variables), no del servicio: para `turn_watch once` y el panel.
func localWarnings(st *State) []string {
	var out []string
	self, _ := os.Executable()
	bin := binDir()
	if _, err := os.Lstat(filepath.Join(bin, "cargo")); err != nil {
		return nil // sin el envoltorio instalado no hay nada que saltarse
	}
	if first, bypass := cargoBypass(os.Getenv("PATH"), bin, self); bypass {
		out = append(out, fmt.Sprintf("en tu PATH, %s va antes que %s: esas compilaciones no pasan por la cola ni por el tope de CPU (solo la pausa las alcanza); pon %s primero",
			tildePath(first), tildePath(filepath.Join(bin, "cargo")), tildePath(bin)))
	} else if first == "" {
		out = append(out, fmt.Sprintf("%s no está en tu PATH: el envoltorio de cargo no se usa", tildePath(bin)))
	}
	if st == nil || st.Kache == nil || st.Kache.State == kacheDisabled || st.Kache.State == kacheMissing {
		return out
	}
	for _, key := range []string{"RUSTC_WRAPPER", "CARGO_BUILD_RUSTC_WRAPPER"} {
		if v, ok := os.LookupEnv(key); ok {
			if w := classifyWrapper(v, key); w.Kind != WrapperKache {
				out = append(out, fmt.Sprintf("en esta shell %s=%q: cargo no usa kache aquí", key, v))
			}
			break
		}
	}
	return out
}
