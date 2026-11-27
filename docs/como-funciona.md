# Cómo funciona turn_watch

[English](how-it-works.md)

turn_watch es un solo binario estático de Go con varios papeles, según su nombre y
su primer argumento:

| Se llama como | Papel |
| --- | --- |
| `turn_watch daemon` | el servicio de fondo (dueño del límite) |
| `turn_watch` | el panel (TUI): solo lee el estado del servicio y le manda pedidos |
| `cargo` (enlace) | el envoltorio de cargo |
| `sudo turn_watch enforce --dir DIR` | el ayudante de root (modo de todo el sistema) |
| `turn_watch on/off/limit/hold/once/…` | pequeños clientes del servicio |

## Archivos

Carpeta de estado: `$XDG_RUNTIME_DIR/turn_watch/` en Linux (o la carpeta temporal si
no está definida), `~/Library/Caches/turn_watch/` en macOS.

| Archivo | Lo escribe | Para qué |
| --- | --- | --- |
| `state.json` | el servicio, cada segundo | todo lo que muestran el panel y la barra |
| `jobserver` | el servicio | la FIFO con los turnos de la cola (solo existe con el límite activo) |
| `queue.json` | el servicio | límite actual y PID del servicio, para el envoltorio y `hold` |
| `queue.lock` | el servicio | `flock`: un solo dueño de la cola |
| `control` | `on`/`off`, el panel | pedido de activar o desactivar el límite |
| `limit.request` | `limit N`, el panel | pedido de cambiar el límite en caliente |
| `stopped.json` | el servicio | los PIDs que pausó, para reanudarlos tras una caída |

El ayudante de root nunca escribe en esa carpeta (que es del usuario): sus archivos
van a `/run/turn_watch/<uid>/` (`/var/run/turn_watch/<uid>/` en macOS), de root y
con modo 0755 para que el servicio pueda leer `helper.pid`:

| Archivo | Lo escribe | Para qué |
| --- | --- | --- |
| `stopped-root.json` | el ayudante de root | los PIDs que pausó, para reanudarlos tras una caída |
| `helper.pid` | el ayudante de root | si el modo de todo el sistema está corriendo |

Todos los archivos de estado se escriben con `writeFileSafe` (la carpeta se abre con
`O_DIRECTORY|O_NOFOLLOW` y se comprueba sobre el descriptor: carpeta de verdad, del
usuario esperado, sin escritura para grupo ni otros; después un temporal de nombre
aleatorio con `openat(O_CREAT|O_EXCL|O_NOFOLLOW)` y `renameat`) y se leen con
`O_NOFOLLOW`: un enlace simbólico en una carpeta de estado no desvía ni una
escritura ni una lectura. El ayudante de root además valida `--dir` (absoluta, que
se llame `turn_watch`, no un enlace, del `SUDO_UID`) y solo abre la FIFO de la cola
si es una FIFO de ese usuario.

Configuración: `config.json` en `~/.config/turn_watch/` (Linux) o
`~/Library/Application Support/turn_watch/` (macOS). El servicio la relee cada
segundo, así que se puede editar a mano.

## La vuelta del servicio

Cada segundo: lee la tabla de procesos, actualiza las compilaciones y los grupos de
agentes, aplica `control`/`limit.request`, (Linux) mete a los agentes en la scope
con tope, escribe `state.json` y avisa en el escritorio de las compilaciones que
duraron 20 s o más. Cada 100 ms mira los `rustc` de cada compilación (la mayoría de
los crates compila en menos de un segundo y si no se perderían) y cada 200 ms aplica
la pausa.

### Compilaciones

Una compilación es un `cargo` raíz (sin otro cargo por encima) y todo lo que cuelga
de él. El proyecto, el worktree de git y la rama salen del directorio de trabajo de
la raíz (subiendo hasta `.git`; los worktrees se reconocen por el archivo
`gitdir:`). Los crates se cuentan por `rustc --crate-name …`; la fase (compilando,
enlazando, build script, C/C++, descargando) por los hijos. En las compilaciones de
Docker (Linux) el cargo corre como root dentro de BuildKit; el proyecto sale del
`Cargo.lock` de la carpeta desde la que corre tu `docker build`/`uc build`.

### Agentes

Los procesos cuyo nombre (o `argv[0]`, o `argv[1]` para scripts de
`node`/`bun`/…) está en `agents` son raíces de agente. Cada proceso de sus
subárboles va al grupo de su propio directorio de trabajo si es un repositorio de
git; si no, al de su padre. La CPU es la diferencia por proceso entre lecturas; en
Linux la CPU de los hijos que murieron entre lecturas se recupera del `cutime` del
padre sin contarla dos veces. Los grupos se suavizan (media móvil exponencial, ~3 s)
y se mantienen 3 minutos después de su último proceso para que la lista no brinque.
Las compilaciones que lanza un agente salen una sola vez, como compilaciones.

## El límite

**Cola.** El servicio crea la FIFO y escribe N−1 turnos (bytes `+`); cada cargo
tiene un turno implícito, según el protocolo del jobserver de GNU make. El
envoltorio pone `CARGO_MAKEFLAGS="-jN --jobserver-auth=fifo:RUTA"`, quita
`CARGO_BUILD_JOBS` y quita también el `-j` de la línea de comandos (`-jN`, `-j N`,
`--jobs N`, `--jobs=N`): un `-j` explícito le gana al jobserver y ese cargo abriría
su propio paralelismo, tantas veces como compilaciones haya a la vez. Lo que va
después de `--` es del programa, no de cargo, y se deja igual. Subir el límite
escribe más turnos; bajarlo anota una deuda y un
lector retira turnos a medida que vuelven. Si nada compila durante 3 s se rellena
el fondo (un proceso que murió con turnos en la mano los pierde). Al desactivar el
límite se escriben turnos de sobra (lo que esperaba sigue como una compilación sin
límite) y se borra la FIFO, para que los cargo nuevos no la usen.

**Pausa.** Los trabajos son los procesos `rustc`, `cc1`, `cc1plus` y
`build-script-*` que tienen un cargo entre sus antepasados. Ordenados por inicio,
corren los N primeros y el resto recibe `SIGSTOP`. Cuando uno termina, el pausado
más antiguo recibe `SIGCONT`. El servicio nunca pausa otra cosa (agentes, tests,
servidores). Los PIDs pausados se guardan con su hora de inicio y su dueño, así
nunca se manda una señal a un PID reciclado; al arrancar el servicio o el ayudante
de root, se reanuda lo que haya quedado de una caída, pero solo los PIDs que siguen
con la hora de inicio y el dueño anotados, están en pausa y son un trabajo bajo un
cargo (no se mira el cgroup: la pausa alcanza a propósito compilaciones fuera de la
slice, como las de Docker).

**Modo de todo el sistema.** El panel pide sudo una vez (`sudo -v` en la terminal)
y lanza `sudo turn_watch enforce --dir DIR --detach`, que hace la misma pausa como
root para los trabajos de todos los usuarios, lee el límite de `queue.json` y
termina al desactivarse el límite. Mientras corre, el servicio deja de pausar por
su cuenta.

**Tope de CPU (solo Linux).** `turn_watch.slice` es una slice de systemd de usuario
(`CPUWeight=50`). El servicio le pone `CPUQuota=N×100%` con el límite activo y se lo
quita al desactivarlo. El envoltorio corre cargo con
`systemd-run --user --scope --slice=turn_watch.slice`. Los agentes se mueven con
`StartTransientUnit` / `AttachProcessesToUnit` (D-Bus con `busctl`) a
`turn_watch-agentes.scope` dentro de esa slice; los procesos fuera de tu
`user@.service` (una sesión de SSH, otro usuario) no se pueden mover y se avisa una
vez en el registro. Con `agents_cap: false` se sacan a
`app.slice/turn_watch-agentes-libre.scope`.

## kache

`turn_watch install` baja kache en la versión fijada en `kache.go` (`kacheVersion`)
y la verifica contra el SHA-256 fijado ahí para cada plataforma; `kache init -y
--no-shell` corre solo si cargo no tiene ya un `rustc-wrapper` (de `RUSTC_WRAPPER`,
`CARGO_BUILD_RUSTC_WRAPPER` y `$CARGO_HOME/config[.toml]`, leído como TOML). Lo que
falla queda en `kache-setup-error.txt`, junto a `config.json`.

El servicio revisa kache en una goroutine al arrancar y cada 30 s:
`kache --json daemon status`, luego `kache --json stats --since 24h` solo si el
daemon corre, y `kache --json doctor` cada 30 min. Cada respuesta es el documento
JSON versionado de kache (`schema_version` 1); un campo que falta, otra versión del
esquema o `success: false` se vuelven un estado explícito "no disponible: motivo"
en `state.json` (`kache`), nunca ceros.

El progreso de una compilación cuenta crates *listos*, compilados o de la caché: lo
mayor entre las huellas de cargo (`<target>/[<triple>/]<perfil>/.fingerprint/*/*.json`,
que cargo escribe al terminar cada unidad, haga lo que haga el wrapper) posteriores
al inicio, y los crates vistos en procesos `rustc` o del wrapper (`kache`,
`sccache`) con `--crate-name` (lo único que hay para Docker).

El daemon de kache (`kache.service`) queda fuera de `turn_watch.slice` salvo con
`kache_in_slice` (un drop-in con `Slice=turn_watch.slice`, que se aplica al
reiniciar `kache.service`). Las compilaciones de un fallo de caché las lanza el
cliente `kache` bajo cargo, así que están en la slice y bajo la pausa igual; el
planificador propio de kache también puede retener compiladores tras un fallo.

## macOS

| Linux | macOS |
| --- | --- |
| `/proc/<pid>/stat`, `cmdline` | `sysctl kern.proc.all` (pid, ppid, uid, nombre, estado, inicio) + `sysctl kern.procargs2` (argv exacto) |
| CPU y RSS de `/proc` | un `ps -axww -o pid=,ppid=,uid=,rss=,%cpu=,time=,state=,args=` por segundo (no en la pausa de 200 ms) |
| `/proc/<pid>/cwd` para cualquier proceso | `lsof -a -d cwd -Fn -p PID`, solo para las raíces (cargo y agentes), con caché de 30 s por PID |
| `cutime` de los hijos muertos | no existe: lo que un proceso gastó entre la última lectura y su muerte se pierde |
| CPU de la máquina de `/proc/stat` | suma del `%cpu` de los procesos según `ps` |
| memoria de `/proc/meminfo` | `hw.memsize` + `vm_stat` (apps + fija + comprimida, como el Monitor de Actividad) |
| `TIOCINQ` sobre la FIFO | `FIONREAD` |
| servicio de systemd de usuario | LaunchAgent (`launchctl bootstrap gui/$UID`) |
| `notify-send` | `osascript` (`display notification`) |
| slice + `CPUQuota` | **nada**: solo cola y pausa; el envoltorio pone `nice 10` a cargo |

El código de procesos está en `proc_linux.go` / `proc_darwin.go`; el servicio y el
tope, en `service_linux.go` + `agentcap_linux.go` / `service_darwin.go`; los ioctl
de la terminal, en `tty_*.go`. Los lectores de la salida de `ps`, `lsof`, `vm_stat` y
`kern.procargs2` están en `psparse.go` y tienen pruebas que corren en cualquier
sistema.

## CI y releases

`.github/workflows/ci.yml` corre `go vet` y las pruebas en Ubuntu y macOS, una
prueba de humo del servicio de verdad en los dos (en macOS también
`turn_watch install` con launchd en un `HOME` temporal) y compila los cuatro
binarios. Al empujar una etiqueta `v*` corre `.github/workflows/release.yml`, que
los compila con `scripts/build.sh` (`CGO_ENABLED=0`, `-X main.version=<etiqueta>`),
empaqueta `turn_watch-<os>-<arch>.tar.gz` más `SHA256SUMS` y crea la release de
GitHub.
