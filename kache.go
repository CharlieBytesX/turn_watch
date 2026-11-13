package main

// kache (caché de compilación para Rust y C/C++, https://kunobi.ninja/docs/kache) es
// parte de la instalación de turn_watch:
//   1. si ya está (en el PATH o en ~/.cargo/bin), se usa;
//   2. si no, con cargo-binstall (binario oficial, unos segundos);
//   3. si tampoco hay binstall, se baja el binario oficial de GitHub, se verifica su
//      SHA-256 y se deja en ~/.local/bin.
// Después se configura con `kache init -y --no-shell`: solo la configuración de cargo
// (rustc-wrapper), sin tocar la shell, porque el envoltorio de cargo es el de turn_watch.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// setupKache instala (si hace falta) y configura kache. Errores: aviso, nunca fatal.
func setupKache() {
	start := time.Now()
	kache := findKache()
	how := "ya estaba"
	if kache == "" {
		var err error
		if kache, how, err = installKache(); err != nil {
			warn("kache: no se pudo instalar (%v); se sigue sin caché", err)
			return
		}
	}
	if kacheConfigured() {
		step("caché de compilación: kache (%s, ya configurado)", how)
		return
	}
	cmd := exec.Command(kache, "init", "-y", "--no-shell")
	if out, err := cmd.CombinedOutput(); err != nil {
		warn("kache init falló: %s", lastLine(string(out)))
		return
	}
	step("caché de compilación: kache (%s, configurado en %s)", how, time.Since(start).Round(100*time.Millisecond))
}

// kacheConfigured: cargo ya usa kache como rustc-wrapper.
func kacheConfigured() bool {
	b, err := os.ReadFile(filepath.Join(home(), ".cargo", "config.toml"))
	return err == nil && strings.Contains(string(b), `rustc-wrapper = "kache"`)
}

func installKache() (path, how string, err error) {
	if bin := cargoBinstall(); bin != "" {
		cmd := exec.Command(bin, "kache", "--no-confirm", "--disable-strategies", "compile")
		if out, err := cmd.CombinedOutput(); err == nil {
			if p := findKache(); p != "" {
				return p, "instalado con cargo-binstall", nil
			}
		} else {
			warn("cargo-binstall falló (%s); se baja de GitHub", lastLine(string(out)))
		}
	}
	p, err := downloadKache()
	if err != nil {
		return "", "", err
	}
	return p, "bajado de GitHub", nil
}

func cargoBinstall() string {
	if p, err := exec.LookPath("cargo-binstall"); err == nil {
		return p
	}
	p := filepath.Join(home(), ".cargo", "bin", "cargo-binstall")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// downloadKache baja el último release para esta plataforma y verifica su SHA-256.
func downloadKache() (string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	triple := map[string]string{"linux": "unknown-linux-musl", "darwin": "apple-darwin"}[runtime.GOOS]
	if arch == "" || triple == "" {
		return "", fmt.Errorf("no hay binario de kache para %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := getJSON(client, "https://api.github.com/repos/"+kacheRepo+"/releases/latest", &rel); err != nil || rel.Tag == "" {
		return "", fmt.Errorf("versión de kache: %v", err)
	}
	name := fmt.Sprintf("kache-%s-%s.tar.gz", arch, triple)
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s/", kacheRepo, rel.Tag)

	sumText, err := getText(client, base+name+".sha256")
	if err != nil {
		return "", fmt.Errorf("sha256: %w", err)
	}
	want := strings.Fields(sumText)
	if len(want) == 0 {
		return "", fmt.Errorf("sha256 vacío")
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
	if got := hex.EncodeToString(h.Sum(nil)); got != want[0] {
		return "", fmt.Errorf("el SHA-256 no coincide (%s ≠ %s)", got, want[0])
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

func getJSON(c *http.Client, url string, v any) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
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
