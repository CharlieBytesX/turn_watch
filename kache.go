package main

// kache (caché de compilación para Rust y C/C++, https://kunobi.ninja/docs/kache) es
// parte de la instalación de turn_watch:
//   1. si ya está (en el PATH, en ~/.cargo/bin o en ~/.local/bin), se usa;
//   2. si no, se baja el binario oficial de GitHub de una versión FIJADA aquí
//      (kacheVersion) y se verifica contra el SHA-256 fijado aquí para cada
//      plataforma (kacheSHA256), no contra el .sha256 del mismo release: si alguien
//      cambia el release, la instalación falla en vez de instalar otra cosa. Otra
//      versión solo a propósito: TURN_WATCH_KACHE_VERSION=vX.Y.Z junto con
//      TURN_WATCH_KACHE_SHA256=<hex del .tar.gz de esta plataforma> (o "release" para
//      aceptar, sabiéndolo, el .sha256 publicado en ese release).
//      cargo-binstall ya no se usa: baja la última versión sin un hash fijado.
// Después se configura con `kache init -y --no-shell` (solo rustc-wrapper en la
// configuración de cargo, sin tocar la shell: el envoltorio de cargo es el de
// turn_watch), pero solo si cargo no usa ya otro rustc-wrapper (cargoconf.go): uno
// que ya exista (sccache, una ruta propia, RUSTC_WRAPPER) no se pisa; se avisa.
// Lo que falle queda en kache-setup-error.txt y el servicio lo sigue mostrando.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const kacheRepo = "kunobi-ninja/kache"

// kacheVersion: la versión que instala turn_watch si kache no está.
const kacheVersion = "v2.2.0"

// kacheSHA256: SHA-256 de cada kache-<arch>-<triple>.tar.gz de kacheVersion,
// calculados al fijar la versión (y comparados con los .sha256 del release).
var kacheSHA256 = map[string]string{
	"x86_64-unknown-linux-musl":  "eeb6e72b9f55d0e9d61cae0789ffa8f3ab8c33bef166b7af655cc0806447ebac",
	"aarch64-unknown-linux-musl": "99c74e9332a92e8f06d6fbdb077aff5365f364d5b5e6d69cf013fa616e887f22",
	"x86_64-apple-darwin":        "cb76efd5c23023bff27cb2b7d787b139b18fd0c851dda84f45b5f71128c38b0e",
	"aarch64-apple-darwin":       "8096492dc878796db6a79fef05f3e15ac8af450059c59db1f546eec00d81c3f5",
}

// kacheAsset: el triple de esta plataforma ("" si no hay binario de kache).
func kacheAsset(goos, goarch string) string {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	triple := map[string]string{"linux": "unknown-linux-musl", "darwin": "apple-darwin"}[goos]
	if arch == "" || triple == "" {
		return ""
	}
	return arch + "-" + triple
}

// kachePin: qué versión bajar y con qué SHA-256 verificarla. want == "release":
// aceptar el .sha256 publicado (solo si se pidió con TURN_WATCH_KACHE_SHA256=release).
func kachePin(asset, version, sum string) (tag, want string, err error) {
	if version == "" || version == kacheVersion {
		if sum != "" && sum != kacheSHA256[asset] {
			return "", "", fmt.Errorf("TURN_WATCH_KACHE_SHA256 no coincide con el hash fijado de kache %s", kacheVersion)
		}
		if kacheSHA256[asset] == "" {
			return "", "", fmt.Errorf("no hay hash fijado de kache %s para %s", kacheVersion, asset)
		}
		return kacheVersion, kacheSHA256[asset], nil
	}
	if !strings.HasPrefix(version, "v") || strings.ContainsAny(version, "/ ?#") {
		return "", "", fmt.Errorf("TURN_WATCH_KACHE_VERSION no válida: %q (p. ej. v2.2.0)", version)
	}
	switch {
	case sum == "":
		return "", "", fmt.Errorf("TURN_WATCH_KACHE_VERSION=%s necesita TURN_WATCH_KACHE_SHA256 (el hash del .tar.gz de %s, o \"release\" para aceptar el del release)", version, asset)
	case sum == "release":
		return version, "release", nil
	case len(sum) != 64 || strings.Trim(strings.ToLower(sum), "0123456789abcdef") != "":
		return "", "", fmt.Errorf("TURN_WATCH_KACHE_SHA256 no es un SHA-256 en hex")
	}
	return version, strings.ToLower(sum), nil
}

// findKache: ruta de kache si ya está instalado.
func findKache() string {
	if p, err := exec.LookPath("kache"); err == nil {
		return p
	}
	for _, p := range []string{
		filepath.Join(home(), ".cargo", "bin", "kache"),
		filepath.Join(binDir(), "kache"),
	} {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// setupKache instala (si hace falta) y configura kache. Los errores no son fatales,
// pero quedan anotados (recordSetupError) y el servicio los sigue mostrando.
func setupKache() {
	start := time.Now()
	kache := findKache()
	how := "ya estaba"
	if kache == "" {
		var err error
		if kache, err = downloadKache(); err != nil {
			recordSetupError("no se pudo instalar: " + err.Error())
			warn("kache: no se pudo instalar (%v); sin caché (el panel lo seguirá avisando)", err)
			return
		}
		how = "bajado de GitHub, " + kacheInstalledVersion()
	}
	w := detectWrapper(os.LookupEnv, home())
	switch w.Kind {
	case WrapperKache:
		recordSetupError("")
		step("caché de compilación: kache (%s, ya configurado: %s)", how, w.Source)
		return
	case WrapperOther, WrapperOff, WrapperUnknown:
		msg := w.String() + "; no se cambia, así que kache no se configura"
		recordSetupError(msg)
		warn("kache: %s", msg)
		return
	}
	cmd := exec.Command(kache, "init", "-y", "--no-shell")
	if out, err := cmd.CombinedOutput(); err != nil {
		recordSetupError("kache init falló: " + lastLine(string(out)))
		warn("kache init falló: %s", lastLine(string(out)))
		return
	}
	if w := detectWrapper(os.LookupEnv, home()); w.Kind != WrapperKache {
		recordSetupError("después de kache init: " + w.String())
		warn("kache init terminó, pero %s", w.String())
		return
	}
	recordSetupError("")
	step("caché de compilación: kache (%s, configurado en %s)", how, time.Since(start).Round(100*time.Millisecond))
}

func kacheInstalledVersion() string {
	if v := envVar("KACHE_VERSION"); v != "" {
		return v
	}
	return kacheVersion
}

// downloadKache baja kache (kacheVersion, o la pedida a propósito) para esta
// plataforma y lo verifica contra el SHA-256 fijado antes de abrirlo.
func downloadKache() (string, error) {
	asset := kacheAsset(runtime.GOOS, runtime.GOARCH)
	if asset == "" {
		return "", fmt.Errorf("no hay binario de kache para %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	tag, want, err := kachePin(asset, envVar("KACHE_VERSION"), envVar("KACHE_SHA256"))
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	name := "kache-" + asset + ".tar.gz"
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s/", kacheRepo, tag)
	if want == "release" {
		sumText, err := getText(client, base+name+".sha256")
		if err != nil {
			return "", fmt.Errorf("sha256: %w", err)
		}
		f := strings.Fields(sumText)
		if len(f) == 0 {
			return "", fmt.Errorf("sha256 vacío")
		}
		want = strings.ToLower(f[0])
		warn("kache %s: se verifica con el .sha256 del propio release (TURN_WATCH_KACHE_SHA256=release)", tag)
	}
	resp, err := client.Get(base + name)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("descarga: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "kache-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("el SHA-256 de %s no coincide (%s ≠ %s)", name, got, want)
	}

	f, err := os.Open(tmp.Name())
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	bin := binDir()
	_ = os.MkdirAll(bin, 0o755)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		base := filepath.Base(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !strings.HasPrefix(base, "kache") || strings.HasSuffix(base, ".asc") || base != hdr.Name {
			continue
		}
		dst := filepath.Join(bin, base)
		out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, 200<<20)); err != nil {
			out.Close()
			return "", err
		}
		out.Close()
		if err := os.Rename(dst+".tmp", dst); err != nil {
			return "", err
		}
	}
	p := filepath.Join(bin, "kache")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("el paquete no traía kache")
	}
	return p, nil
}

func getText(c *http.Client, url string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return string(b), err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
