package main

// Archivos de estado sin seguir enlaces simbólicos.
//
// El ayudante de root (`sudo turn_watch enforce`) y el servicio escriben y leen
// archivos pequeños (pids pausados, helper.pid, queue.json). Si root escribiera con
// os.WriteFile en una carpeta del usuario, el usuario (o cualquier proceso suyo)
// podría dejar ahí un enlace simbólico y hacer que root sobrescriba cualquier archivo
// del sistema. Por eso:
//
//   - la carpeta se abre una sola vez con O_DIRECTORY|O_NOFOLLOW y se comprueba sobre
//     el descriptor (fstat): es una carpeta, de quien debe ser y nadie más puede
//     escribir en ella;
//   - el archivo se crea con openat(O_CREAT|O_EXCL|O_NOFOLLOW) relativo a ese
//     descriptor (un nombre sin "/"), y se renombra con renameat: no hay ruta que
//     alguien pueda cambiar entre la comprobación y la escritura;
//   - la lectura también va con O_NOFOLLOW y exige un archivo normal del mismo dueño.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// openDirChecked abre dir sin seguir un enlace en su último componente y comprueba
// que sea una carpeta de owner sin escritura para el grupo ni para otros.
func openDirChecked(dir string, owner int) (int, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("%s: %w", dir, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("%s: %w", dir, err)
	}
	if err := checkOwnerMode(dir, st, owner, true); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// checkOwnerMode: el archivo o carpeta es de owner y nadie más puede escribir en él.
func checkOwnerMode(path string, st unix.Stat_t, owner int, wantDir bool) error {
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	isReg := st.Mode&unix.S_IFMT == unix.S_IFREG
	switch {
	case wantDir && !isDir:
		return fmt.Errorf("%s: no es una carpeta", path)
	case !wantDir && !isReg:
		return fmt.Errorf("%s: no es un archivo normal", path)
	case int(st.Uid) != owner:
		return fmt.Errorf("%s: es del usuario %d, no del %d", path, st.Uid, owner)
	case st.Mode&0o022 != 0:
		return fmt.Errorf("%s: el grupo u otros pueden escribir (modo %o)", path, st.Mode&0o777)
	}
	return nil
}

// checkDir: dir es una carpeta de verdad (no un enlace) de owner, sin escritura para
// el grupo ni para otros.
func checkDir(dir string, owner int) error {
	fd, err := openDirChecked(dir, owner)
	if err != nil {
		return err
	}
	unix.Close(fd)
	return nil
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return fmt.Errorf("nombre de archivo no válido: %q", name)
	}
	return nil
}

// writeFileSafe escribe dir/name de forma atómica (temporal + renameat) sin seguir
// enlaces: la carpeta debe ser de owner y no escribible por otros.
func writeFileSafe(dir string, owner int, name string, data []byte, perm os.FileMode) error {
	if err := validName(name); err != nil {
		return err
	}
	dfd, err := openDirChecked(dir, owner)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := "." + name + "." + hex.EncodeToString(rnd[:]) + ".tmp"
	fd, err := unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return fmt.Errorf("%s/%s: %w", dir, tmp, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	// Fchmod: que la umask no deje permisos distintos de los pedidos.
	werr := f.Chmod(perm.Perm())
	if werr == nil {
		_, werr = f.Write(data)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = unix.Renameat(dfd, tmp, dfd, name)
	}
	if werr != nil {
		_ = unix.Unlinkat(dfd, tmp, 0)
		return fmt.Errorf("%s/%s: %w", dir, name, werr)
	}
	return nil
}

// readFileSafe lee dir/name (hasta limit bytes) sin seguir enlaces y solo si la
// carpeta y el archivo son de owner y nadie más puede escribirlos.
func readFileSafe(dir string, owner int, name string, limit int64) ([]byte, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	dfd, err := openDirChecked(dir, owner)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("%s/%s: %w", dir, name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	// El archivo: de owner o de root (lo que dejó un ayudante de root de antes).
	fileOwner := owner
	if int(st.Uid) == rootOwner {
		fileOwner = rootOwner
	}
	if err := checkOwnerMode(dir+"/"+name, st, fileOwner, false); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

// openFifoSafe abre dir/name en solo lectura y sin bloquear, sin seguir enlaces, y
// solo si es una FIFO de owner (la cola). Así root nunca abre otra cosa por error.
func openFifoSafe(dir string, owner int, name string) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	dfd, err := openDirChecked(dir, owner)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFIFO || int(st.Uid) != owner {
		unix.Close(fd)
		return nil, fmt.Errorf("%s/%s: no es la FIFO de la cola", dir, name)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// removeFileSafe borra dir/name (unlinkat no sigue enlaces: borra el enlace mismo).
func removeFileSafe(dir string, owner int, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	dfd, err := openDirChecked(dir, owner)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	if err := unix.Unlinkat(dfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// ensureOwnedDir crea dir (con perm) si falta y comprueba que sea de owner y no
// escribible por otros.
func ensureOwnedDir(dir string, owner int, perm os.FileMode) error {
	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return checkDir(dir, owner)
}
