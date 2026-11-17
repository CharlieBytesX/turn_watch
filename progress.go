package main

// Progreso de una compilación: cuántos crates ya están listos, compilados o sacados
// de la caché.
//
// Antes solo se contaban los rustc vivos que veía el muestreo (cada 100 ms). Con
// kache (u otro rustc-wrapper) un acierto de caché no lanza rustc, y un crate pequeño
// dura menos que el muestreo, así que el panel decía "0–15 de 473" en una
// compilación casi toda de caché. Ahora se cuentan dos cosas y se toma la mayor (las
// dos son cotas por abajo del número real):
//
//  1. Las huellas que escribe cargo al terminar cada unidad, pase lo que pase con el
//     wrapper: <target>/<perfil>/.fingerprint/<crate>-<hash>/<tipo>-<nombre>.json
//     (y <target>/<triple>/<perfil>/… con --target). Una unidad cuenta si ese .json
//     es posterior al inicio del cargo. Es lo más fiable: no depende de kache ni del
//     muestreo y sigue funcionando si kache restaura con enlaces duros (las huellas
//     las escribe cargo, no kache). El formato de .fingerprint es interno de cargo:
//     si cambiara, se cae al punto 2 sin inventar nada.
//     El target se busca como cargo: --target-dir, CARGO_TARGET_DIR del proceso,
//     build.target-dir en .cargo/config(.toml) y si no <workspace>/target. Si dos
//     cargo comparten target a la vez, se cuentan unidades de los dos.
//  2. El muestreo de procesos, ahora también de los procesos del wrapper (kache,
//     sccache) con --crate-name: así cuentan también los aciertos lentos. Es lo único
//     que hay para las compilaciones dentro de Docker (su target no es visible).

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

const fingerprintEvery = 2 * time.Second

// isRustcWrapper: un rustc-wrapper conocido envolviendo un rustc (tiene --crate-name).
func isRustcWrapper(p *proc) bool {
	switch p.comm {
	case "kache", "kache-engine", "sccache":
		return true
	}
	return false
}

// resolveTargetDir: la carpeta target de un cargo raíz.
func resolveTargetDir(args []string, env func(string) string, cwd, cargoHome string) string {
	abs := func(p string) string {
		if filepath.IsAbs(p) || cwd == "" {
			return p
		}
		return filepath.Join(cwd, p)
	}
	if v := argValue(args, "--target-dir"); v != "" {
		return abs(v)
	}
	if v := env("CARGO_TARGET_DIR"); v != "" {
		return abs(v)
	}
	if cwd == "" {
		return ""
	}
	if v := configTargetDir(cwd, cargoHome); v != "" {
		return v
	}
	if dir, _ := cargoLock(cwd); dir != "" {
		return filepath.Join(dir, "target")
	}
	return ""
}

// fingerprintDirs: las carpetas .fingerprint de un target (perfil y triple/perfil).
func fingerprintDirs(target string) []string {
	var out []string
	for _, pat := range []string{"*/.fingerprint", "*/*/.fingerprint"} {
		m, _ := filepath.Glob(filepath.Join(target, pat))
		out = append(out, m...)
	}
	return out
}

// fingerprintDone cuenta las unidades que cargo terminó en target desde since. seen
// guarda las ya contadas (para no volver a mirarlas) y se actualiza.
func fingerprintDone(target string, since time.Time, seen map[string]bool) int {
	for _, fp := range fingerprintDirs(target) {
		units, err := os.ReadDir(fp)
		if err != nil {
			continue
		}
		for _, u := range units {
			key := filepath.Join(fp, u.Name())
			if seen[key] || !u.IsDir() {
				continue
			}
			files, err := os.ReadDir(key)
			if err != nil {
				continue
			}
			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".json") || f.IsDir() {
					continue
				}
				if fi, err := f.Info(); err == nil && !fi.ModTime().Before(since) {
					seen[key] = true
					break
				}
			}
		}
	}
	return len(seen)
}

// updateDone: Done es lo mayor entre lo muestreado y las huellas de cargo.
func (b *Build) updateDone() { b.Done = max(len(b.seen), b.fpDone) }

// scanFingerprints actualiza fpDone si toca (cada fingerprintEvery).
func (b *Build) scanFingerprints(now time.Time) {
	if b.Docker || b.targetDir == "" || now.Sub(b.fpLast) < fingerprintEvery {
		return
	}
	b.fpLast = now
	if b.fpSeen == nil {
		b.fpSeen = map[string]bool{}
	}
	// Un segundo de margen: el inicio sale de ticks del reloj del kernel.
	b.fpDone = fingerprintDone(b.targetDir, b.Started.Add(-time.Second), b.fpSeen)
	b.updateDone()
}
