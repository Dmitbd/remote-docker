# Архитектура Remote Docker

**Статус документа:** Текущее + В активной ветке + Целевое состояние

**Текущее проверено относительно:** `main` @ `3dc60ed`

**Активная ветка проверена относительно:** `fix/desktop-pairing-state` @ `fd6a26f`
**Дата содержательной проверки:** 2026-08-11

## Как читать статусы

- **Текущее** — находится в `main` и подтверждено production-кодом.
- **В активной ветке** — находится только в `fix/desktop-pairing-state`, не является частью `main` и ещё требует независимого review и физической проверки.
- **Целевое состояние** — обязательное свойство рабочего MVP, которое может быть реализовано не полностью.

## Системная граница

Remote Docker переносит Docker Engine и Docker data с Mac на Windows PC, но сохраняет редактор, shell, Git и основную копию source files на Mac.

```text
Mac                                              Windows
---------------------------------------------    --------------------------------------
Remote Docker UI + desktop process               Remote Docker UI + desktop process
local control API                                 private-LAN discovery/pairing endpoint
docker/compose launcher                           authenticated tunnel server :49221
Docker CLI context -> local SSH relay        <->  fixed service bridge into managed WSL2
Syncthing client -> local sync relay         <->  Syncthing in WSL2
localhost forwards for published TCP ports   <->  container published ports in WSL2
                                                  Docker Engine, sshd, remote agent
```

Первая версия имеет фиксированные роли и connection limit `1`: Mac client и Windows host.

## Стек

### Текущее

- Go `1.26` — desktop runtime, CLI, lifecycle, transport, provisioning и tests.
- Wails `v2` — desktop WebView UI.
- HTML, JavaScript и CSS — общий frontend UI.
- `fyne.io/systray` — menu-bar/tray shell.
- `grandcat/zeroconf` — mDNS discovery `_remote-docker._tcp`.
- TLS с Ed25519 identities и pinned peer keys — pairing и tunnel authentication.
- `hashicorp/yamux` — четыре типизированных потока внутри одного TLS tunnel.
- OpenSSH — Docker Context, typed remote RPC и опубликованные TCP forwards.
- Syncthing `2.1.1` — синхронизация зарегистрированных source workspaces.
- WSL2 — изолированное Linux-окружение Windows host.
- ordinary Docker Engine, Docker CLI, Compose и Buildx — контейнерный runtime и клиентские команды.
- macOS Keychain / Windows Credential Manager через `go-keyring` — секреты и identities.

## Компоненты Mac

### Текущее

- `cmd/remote-docker-desktop` — долгоживущий desktop process, local API, lifecycle supervisor и tray/menu-bar integration.
- `cmd/remote-docker-ui` — Wails UI; читает snapshots и отправляет явные операции через local API.
- `cmd/remote-docker` — CLI и launcher для обычных Docker-команд и служебных операций Remote Docker.
- `internal/desktopui` — перевод lifecycle/status в отображаемую модель и защита от одновременного запуска конфликтующих UI-операций.
- `internal/app` — orchestration pairing, connection, synchronization, diagnostics, metrics и cleanup.
- `internal/dockercli` — анализ Docker/Compose invocation, bind sources, build contexts и статических TCP publications.
- `internal/syncer` и local Syncthing runtime — source synchronization.
- `internal/tunnel` — authenticated client session и loopback relays.
- `internal/portrelay` — app-owned SSH forwards для опубликованных container TCP ports.
- `internal/localapi` — локальный control plane между UI/CLI и desktop process.

Обычная Docker-команда проходит preflight, но синтаксис команды не заменяется продуктовым DSL.

## Компоненты Windows

### Текущее

- NSIS installer и PowerShell provisioning устанавливают desktop application и managed WSL2 environment.
- `cmd/remote-docker-desktop` запускается вручную и использует роль Windows host.
- mDNS advertisement публикуется только в активном hosting/pairing состоянии.
- `internal/tunnel.Server` принимает одну authenticated session на TCP `49221`; дополнительный клиент получает busy-состояние.
- `internal/windowsbridge.ServiceDialer` разрешает только фиксированные типы tunnel streams и переводит их на WSL `22` или `22000`.
- WSL address определяется заново для подключения, поэтому изменение внутреннего WSL IP не сохраняется как постоянный публичный endpoint.
- Windows firewall rules должны принадлежать Remote Docker и ограничивать входящий tunnel private-network boundary.

## Управляемое WSL2-окружение

### Текущее

Managed distribution содержит:

- Docker Engine и container runtime;
- OpenSSH server на WSL port `22`;
- Syncthing на WSL port `22000` и loopback control API;
- `remote-docker-remote` для typed RPC: health, presence, diagnostics, recovery, metrics и sync configuration;
- systemd target для управляемых сервисов;
- Docker images, containers, named volumes, databases, writable layers и build cache.

Source workspaces размещаются в Linux filesystem WSL, а не в `/mnt/c`. Тяжёлые Docker data не синхронизируются на Mac.

## Discovery и pairing

### Текущее

1. Windows в hosting/pairing состоянии публикует временную opaque mDNS identity и private-LAN address.
2. Mac выполняет явный поиск и фильтрует неподходящие, публичные и несовместимые records.
3. Pairing bootstrap создаёт временную защищённую session и pinned server identity.
4. Оба UI должны показать участников и один шестизначный comparison code.
5. Windows явно approve или reject запрос.
6. После approve Mac закрепляет tunnel identity, SSH host identity, Syncthing device identity и Docker Context.
7. Секреты хранятся в credential store; public config не должен содержать private keys.

Pairing endpoint использует тот же внешний port `49221`, но отдельный TLS ALPN от рабочего tunnel.

### В активной ветке

`fix/desktop-pairing-state` добавляет background reconciliation, side-effect-free observation, durable rollback journal, revocation proof, pairing generation, completion/cleanup leases, ownership token Docker Context, cross-process file locks и explicit upgrade gate. Эти механизмы пока не являются контрактом `main`.

Последний local HEAD `fd6a26f` закрывает известные review findings по startup gap, проверке shutdown updater/installer и продлению completion lease. Для него ещё не зафиксированы независимое принятие и физический Mac↔Windows результат.

## Рабочий tunnel

### Текущее

Tunnel использует pinned TLS identities, ALPN `remote-docker-tunnel/1` и одну yamux session. Поддерживаются только четыре stream kinds:

| Stream | Mac loopback | Windows/WSL destination |
|---|---:|---:|
| `docker-ssh` | `127.0.0.1:49222` | WSL SSH `22` |
| `workspace-sync` | `127.0.0.1:49220` | WSL Syncthing `22000` |
| `control` | `127.0.0.1:49223` | WSL SSH `22` |
| `metrics` | `127.0.0.1:49224` | WSL SSH `22` |

Tunnel не принимает caller-provided destination и не является универсальным LAN proxy. Если фиксированный Mac loopback port занят чужим процессом, соединение не должно бесконечно повторять запуск или завершать чужой процесс.

## Поток Docker-команды

### Текущее

1. Mac launcher получает исходные аргументы Docker CLI.
2. `internal/dockercli` определяет, нужен ли Engine, какие local bind sources и статические TCP ports затронуты.
3. Bind source обязан находиться внутри явно зарегистрированного workspace.
4. Для bind source выполняются configure, scan и readiness обеих Syncthing сторон.
5. Для статического local port выполняется conflict probe.
6. Реальный Docker CLI запускается с managed context.
7. Context использует pinned SSH alias, направленный на `127.0.0.1:49222`; запрос попадает в Docker socket через WSL SSH.

Endpoint overrides, которые обходят managed identity, запрещаются launcher policy.

## Поток синхронизации workspace

### Текущее

- Пользователь явно регистрирует Mac directory ниже разрешённой root.
- Canonical path и membership проверяются до Docker execution.
- Mac и WSL Syncthing identities создаются и хранятся отдельно от public config secrets.
- Remote typed RPC настраивает только требуемые folder/device records.
- Preflight ждёт готовность обеих сторон перед Docker-командой с bind mount.
- Named volumes и Docker data не являются workspace и не синхронизируются.

## Поток опубликованного TCP-порта

### Текущее

- Docker events и read-only Docker queries формируют desired snapshot опубликованных ports.
- Поддерживается только TCP и loopback Mac destination.
- На каждый desired port запускается принадлежащий приложению OpenSSH local forward через managed alias.
- Конфликт с чужим Mac process возвращается как ошибка; чужой process не останавливается.
- Удаление container publication завершает только принадлежащий приложению forward.
- UDP и host networking фиксируются как неподдерживаемые.

## Состояния приложения

### Текущее

Lifecycle machine содержит состояния:

- `paused`;
- `client_ready`;
- `searching`;
- `host_waiting`;
- `pairing`;
- `pairing_cancellation_pending`;
- `connecting`;
- `connected`;
- `reconnecting`;
- `stopping`;
- `needs_action`.

UI не определяет состояние самостоятельно: он читает local API snapshot и отправляет команды. Discovery вызывается только в `searching`. Closing window сохраняет tray/menu-bar application, а Finish work вызывает terminal shutdown.

## Данные, keys и ownership

### Текущее

На Mac:

- public config содержит paired device metadata и workspaces;
- credential store содержит private tunnel/SSH/Syncthing material;
- managed SSH config и known-host entry относятся к paired device;
- source files остаются основной пользовательской копией.

На Windows/WSL:

- Windows хранит host identity и pairing registry;
- WSL хранит authorized SSH identity, Syncthing configuration и managed Docker data;
- installer хранит application/data roots и владеет только своими shortcuts/firewall rules.

### В активной ветке

Pairing generation, revocation proof, rollback stages, cleanup lease и Docker Context owner token делают cleanup restart-safe и cross-process-aware. До merge эти поля нельзя считать форматом текущего `main` config.

## Lifecycle операций

### Текущее

- **Start client/hosting** запускает session-owned runtime только после явного действия.
- **Pause** останавливает runtime и возвращает приложение в `paused`, но не удаляет доверие.
- **Disconnect** завершает текущую connection session, сохраняя trusted peer.
- **Forget** удаляет выбранное доверие и принадлежащие ему local artifacts; remote revoke может потребовать доступный Windows peer.
- **Finish work** переводит lifecycle в terminal stopping, завершает runtime, relays, child processes и desktop/UI shell.
- **Close window** не равен Finish work и оставляет tray/menu-bar application доступным.

Приложение не регистрирует autostart. После reboot его запускают вручную.

## Security boundaries

### Текущее

- Docker API не публикуется через `2375/2376` в LAN.
- Рабочий LAN listener ограничен tunnel port `49221` и authenticated peer identity.
- System transport принимает только private IP targets и разрешённые ports.
- Windows bridge поддерживает фиксированные SSH/Syncthing destinations, а не произвольный forwarding.
- Pairing требует сравнения short code и явного Windows approval.
- Host identity и tunnel public key закрепляются после pairing.
- Private keys не должны попадать в public config, logs или diagnostics.
- Удаление и uninstall не должны изменять не принадлежащие приложению paths, rules, contexts и distributions.

## Архитектурные инварианты

| Инвариант | Статус |
|---|---|
| Один Mac client и один Windows host в MVP | Текущее |
| Нет незащищённого Docker API в LAN | Текущее |
| Необратимые pairing/cleanup действия имеют явный owner и durable intent | В активной ветке |
| Status/snapshot не выполняет скрытое completion или revoke | В активной ветке |
| Cleanup идемпотентен и не изменяет чужие ресурсы | Текущее частично; усилено в активной ветке |
| Независимые cleanup stages имеют независимые bounded contexts | В активной ветке |
| Finish work прекращает всю принадлежащую приложению фоновую работу | Целевое; требует физической проверки |
| Документы не выдают автоматические tests за физическое доказательство | Текущее правило проекта |

## Известные ограничения

- Физическая готовность `main` не подтверждена на полном Mac↔Windows сценарии.
- Только один trusted Mac.
- Только private LAN; Internet mode отсутствует.
- Только TCP publications; UDP и host networking не поддерживаются.
- Workspaces первой версии ограничены Mac paths ниже `/Users`.
- Packages unsigned и требуют checksum/manifest verification.
- Docker CLI не предоставляет абсолютный atomic compare-and-swap для context относительно произвольного внешнего CLI process; приложение использует ownership checks и fail-closed policy в активной ветке.
- Влияние VPN/security software и длительные resource leaks требуют физической проверки.
