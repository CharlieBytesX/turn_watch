# turn_watch

[English](README.md) · **Español**

Que las compilaciones de Rust —y todo lo que lanzan tus agentes de código— no se
coman la laptop. `turn_watch` muestra cada compilación de `cargo`/`rustc` que corre
en la máquina (también dentro de `docker build`) y todo lo que arrancan los agentes
de código (Claude Code, Codex, opencode, aider, Gemini CLI…), agrupado por
proyecto, worktree de git y rama, y le pone un **límite de núcleos** a todo:

- **Cola de núcleos**: todas las compilaciones comparten un mismo fondo de N
  «turnos» (un jobserver de GNU make). Cinco `cargo test` a la vez de cinco agentes
  usan N núcleos entre todos, no 5 × todos los núcleos.
- **Pausa**: los trabajos de compilación que pasan del límite (`rustc`, `cc1`,
  build scripts, incluso los que no usan la cola) se pausan con `SIGSTOP` y se
  reanudan en orden con `SIGCONT`. Primero los más antiguos; nunca queda nada en
  pausa.
- **Tope de CPU** (Linux): compilaciones y agentes corren en una slice de systemd
  con `CPUQuota = N × 100 %`.
- **Caché de compilación**: instala y configura
  [kache](https://kunobi.ninja/docs/kache) para que los crates recompilados salgan
  de la caché.
- Un panel en la terminal, avisos de escritorio al terminar las compilaciones
  largas y un indicador en la barra de [Omarchy](https://omarchy.org).

El límite lo mantiene un pequeño servicio de fondo: sigue activo aunque cierres el
panel, hasta que lo desactives. La interfaz (panel y mensajes) está en español.

> **El soporte de macOS es nuevo** y por ahora solo se probó en el CI (runners de
> macOS de GitHub Actions). En macOS el límite es solo la cola y la pausa: no hay
> cgroups, así que **no hay tope de CPU**. Ver [macOS](#macos).

## Instalación

```sh
curl -fsSL https://raw.githubusercontent.com/CharlieBytesX/turn_watch/main/install.sh | sh
```

El script detecta el sistema y la arquitectura (Linux/macOS, amd64/arm64), baja la
última release, verifica su SHA-256 contra el `SHA256SUMS` de la release, deja el
binario en `~/.local/bin` y corre `turn_watch install`, que prepara:

- `~/.local/bin/turn_watch` y `~/.local/bin/cargo` → `turn_watch` (el envoltorio
  de cargo; ver [preguntas](#por-qué-un-envoltorio-de-cargo));
- el servicio de fondo: `turn_watch.service` + `turn_watch.slice` (unidades de
  systemd de usuario) en Linux, un LaunchAgent en macOS;
- kache (lo detecta, o baja su release oficial de GitHub en una versión fijada en
  turn_watch y la verifica contra un SHA-256 también fijado en turn_watch) y
  `kache init`, que pone `rustc-wrapper` en `$CARGO_HOME/config.toml`, salvo que
  cargo ya use otro `rustc-wrapper` (sccache, `RUSTC_WRAPPER`…), que nunca se pisa.
  Para saltarlo: `TURN_WATCH_SIN_KACHE=1`; otra versión, a propósito:
  `TURN_WATCH_KACHE_VERSION=vX.Y.Z` con `TURN_WATCH_KACHE_SHA256=<sha256 del .tar.gz>`
  (o `=release` para aceptar el `.sha256` de ese release);
- en Omarchy, el indicador de la barra.

`~/.local/bin` tiene que ir **antes** que `~/.cargo/bin` (y que shims como los de
mise o asdf) en el `PATH`; el instalador, `turn_watch once` y el panel avisan si otro
`cargo` va primero.

Opciones: `TURN_WATCH_VERSION=v0.1.0` instala esa etiqueta; `TURN_WATCH_NO_SETUP=1`
solo deja el binario. También puedes bajar un tar.gz de
[Releases](https://github.com/CharlieBytesX/turn_watch/releases) y correr
`./turn_watch install`. Desde el código (Go, ver `go.mod`):

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' && ./turn_watch install
```

## Uso

```sh
turn_watch            # abre el panel (activa el límite si estaba apagado)
```

El panel muestra la CPU y la RAM repartidas en *compilaciones / agentes / otros*,
la cola y una fila por compilación y por proyecto/worktree de agente. Teclas:

| Tecla | Acción |
| --- | --- |
| `↑` `↓` / `k` `j` / rueda | elegir fila |
| `Enter` | desplegar / plegar el detalle |
| `+` / `-` | subir / bajar el límite (al instante) |
| `d` / `a` | desactivar / activar el límite |
| `s` | modo de todo el sistema (pide sudo) |
| `t` | alternar el tope de CPU de los agentes (Linux) |
| `q`, `Esc`, `Ctrl-C` | cerrar el panel (el límite sigue) |

Comandos:

| Comando | Qué hace |
| --- | --- |
| `turn_watch on` / `off` | activar / desactivar el límite sin abrir el panel |
| `turn_watch limit [N]` | ver o cambiar el límite (núcleos), en caliente |
| `turn_watch sistema on\|off` | `on`: también root, Docker y otros usuarios (sudo); `off`: solo tu usuario |
| `turn_watch hold N -- cmd…` | reservar N turnos de la cola mientras corre `cmd` (para lo que no habla el protocolo del jobserver, como `docker build`) |
| `turn_watch once` | el estado actual, una vez, en texto plano |
| `turn_watch install` / `uninstall` | instalar / quitar todo para el usuario actual |
| `turn_watch version` | la versión |
| `turn_watch help` | la ayuda completa |

El límite por defecto es todos los núcleos menos dos. La configuración está en
`config.json` (`~/.config/turn_watch/` en Linux,
`~/Library/Application Support/turn_watch/` en macOS):

```json
{
  "slots": 10,
  "active": true,
  "system": false,
  "agents": ["claude", "codex", "opencode", "pi", "ante", "aider", "gemini", "cursor-agent", "goose"],
  "agents_cap": true,
  "kache": true,
  "kache_in_slice": false
}
```

`agents` es la lista de nombres de proceso que cuentan como agentes de código;
`agents_cap: false` deja a los agentes sin tope (Linux). `kache: false` apaga la
caché (y sus avisos). `kache_in_slice: true` (Linux) hace que `turn_watch install`
agregue un drop-in para que `kache.service` corra dentro de `turn_watch.slice` y su
daemon comparta el tope de CPU; se aplica al reiniciar `kache.service` (ver
[kache](#kache)).

### kache

La línea **Caché** del panel y `turn_watch once` muestran la salud de kache y sus
últimas 24 h, de `kache --json stats --since 24h` cada 30 s (solo con su daemon vivo:
`kache stats` arrancaría uno) y `kache --json doctor` cada 30 min:

```
Caché kache 82 % aciertos (1230/1500 en 24h) · ahorró 1h12m · almacén 3.2 GB de 10.0 GB · daemon ● 2.1.1
      locales 1180 · prefetch 40 · remotos 10 · duplicados 20 · fallos 250 · 6120 entradas
      desde 2026-10-02: 48211 aciertos, ahorró 42h00m
```

Si algo anda mal dice qué (no instalado, cargo usa otro wrapper, daemon parado,
estadísticas no disponibles y por qué, problemas de doctor, último error) en vez
de un número. Dos cosas a saber: el daemon de kache (`kache.service`) corre por
defecto fuera de `turn_watch.slice`, así que su propia CPU (hashes, restauraciones,
subidas) no tiene tope —las compilaciones de un fallo de caché siguen corriendo
bajo cargo, dentro de la slice—; y kache tiene su propio planificador, que dosifica
los compiladores tras un fallo además de la cola de turn_watch.

## Cómo funciona

Más detalle en [docs/como-funciona.md](docs/como-funciona.md).

- **Monitor.** Cada segundo el servicio lee la tabla de procesos (`/proc` en Linux;
  `sysctl` + una llamada a `ps` en macOS), encuentra los `cargo` raíz con sus
  `rustc`, build scripts y enlazadores, y los árboles de procesos de los agentes.
  Cada compilación saca de git su proyecto, worktree y rama; lo de cada agente se
  agrupa por el directorio de trabajo de sus procesos. El estado va a
  `$XDG_RUNTIME_DIR/turn_watch/state.json` para el panel y la barra.
- **Cola (jobserver).** Con el límite activo, el servicio es dueño de una FIFO con
  N−1 turnos (`$XDG_RUNTIME_DIR/turn_watch/jobserver`). El envoltorio se la pasa al
  cargo de verdad con `CARGO_MAKEFLAGS=--jobserver-auth=fifo:…`: cada `rustc` toma
  un turno antes de arrancar y lo devuelve al terminar, entre todas las
  compilaciones a la vez. Al bajar el límite, los turnos sobrantes se retiran a
  medida que vuelven. El envoltorio además quita de la línea de comandos el
  paralelismo explícito (`-j N`, `-jN`, `--jobs N`, `--jobs=N`): un `-j` puesto a
  mano le gana al jobserver, así que `cargo build -j 24` abriría 24 por su cuenta y
  se saltaría la cola. Lo que va después de `--` es del programa y no se toca.
- **Pausa.** Cinco veces por segundo el servicio cuenta los trabajos de
  compilación bajo cualquier cargo y pausa (`SIGSTOP`) los más nuevos que pasen del
  límite; los reanuda (`SIGCONT`) cuando hay sitio. Así alcanza también a las
  compilaciones que no pasan por el envoltorio. Los pids pausados se guardan en
  disco y se reanudan si el servicio muere. Sin sudo solo puede pausar tus
  procesos; con el modo de todo el sistema, un pequeño ayudante de root
  (`sudo turn_watch enforce`) lo hace para todos los usuarios, Docker incluido.
- **Tope de CPU (Linux).** El envoltorio corre cargo dentro de `turn_watch.slice`
  (`systemd-run --user --scope`) y el servicio le pone a la slice
  `CPUQuota` = N × 100 %. Con el límite activo, los agentes y todos sus
  descendientes se mueven (por la API D-Bus de systemd, sin reiniciar nada) a
  `turn_watch.slice/turn_watch-agentes.scope`: lo que lancen después nace ahí y
  comparte el mismo tope.

### macOS

macOS no tiene `/proc` ni cgroups. Lo que cambia:

- Los procesos salen de `sysctl kern.proc.all`; la CPU y la memoria, de una llamada
  a `ps` por segundo; los argumentos exactos, de `sysctl kern.procargs2`. El
  directorio de trabajo de un proceso se lee con `lsof`, que es lento: solo para
  las raíces (cargo y agentes) y con caché; los procesos hijos aparecen bajo su
  padre.
- **El límite es solo la cola y la pausa. No hay tope de CPU**, ni para las
  compilaciones ni para los agentes (los agentes se ven, no se limitan). Además,
  con el límite activo el envoltorio baja la prioridad de cargo (`nice 10`): eso
  solo cede la CPU a otros programas cuando compiten por ella; no limita nada.
- El servicio es un LaunchAgent
  (`~/Library/LaunchAgents/io.github.charliebytesx.turn_watch.plist`, registro en
  `~/Library/Logs/turn_watch.log`); los avisos usan `osascript`. El estado vive en
  `~/Library/Caches/turn_watch/`.
- Las compilaciones de Docker Desktop corren dentro de su máquina virtual Linux:
  no se ven ni se limitan.
- No hay indicador de Omarchy, claro.

## Requisitos

- Linux (amd64/arm64) con sesión de systemd de usuario para el servicio y el tope
  de CPU (sin systemd puedes correr `turn_watch daemon` tú mismo: tienes la cola y
  la pausa, sin tope), o macOS (Apple Silicon/Intel).
- Un Rust reciente: cargo tiene que entender `--jobserver-auth=fifo:` (cualquier
  versión de 2024 en adelante).
- `sudo` solo para el modo de todo el sistema.

## Desinstalar

```sh
turn_watch uninstall
rm ~/.local/bin/turn_watch
```

Quita el servicio, el envoltorio de cargo (y devuelve el `~/.local/bin/cargo` que
hubiera antes) y el indicador de la barra. La configuración y, si quieres quitar
también kache, la línea `rustc-wrapper = "kache"` de `~/.cargo/config.toml` quedan
para que las borres tú.

## Preguntas frecuentes

### ¿Por qué pide sudo?

Solo para el modo de todo el sistema (`turn_watch sistema on` o `s` en el panel).
Sin él, turn_watch solo puede pausar tus procesos. Las compilaciones que corren
como root —BuildKit de Docker, `sudo cargo`— o como otro usuario necesitan un
ayudante de root para pausarse. El ayudante solo pausa y reanuda trabajos de
compilación y termina al desactivar el límite. Todo lo demás corre como tu usuario.

### ¿Funciona con Docker?

En Linux se ve el `cargo` de dentro de `docker build` (el proyecto sale del
`Cargo.lock` de la carpeta desde la que lanzaste `docker build`). Corre como root,
así que solo se pausa en el modo de todo el sistema, y no usa el jobserver;
envuélvelo con `turn_watch hold N -- docker build …` para reservarle N turnos. En
macOS las compilaciones de Docker corren en una máquina virtual y no se ven.

### ¿Por qué un envoltorio de cargo?

La cola solo funciona si cargo recibe el jobserver, y el tope de CPU solo si cargo
arranca dentro de la slice. Un enlace `~/.local/bin/cargo` → `turn_watch` hace las
dos cosas sin alias ni configuración de la shell: con el límite activo pone
`CARGO_MAKEFLAGS` y, en Linux, corre cargo con `systemd-run --scope`; sin límite
simplemente hace `exec` del cargo de verdad (el primero del `PATH` que no sea él
mismo, o `~/.cargo/bin/cargo`). Las compilaciones que no pasan por él las sigue
alcanzando la pausa.

### ¿Cerrar el panel desactiva el límite?

No. El servicio lo mantiene hasta `turn_watch off` o `d` en el panel. Abrir el
panel lo activa.

## Licencia

[MIT](LICENSE)
