# Архитектура Remote Docker

**Статус документа:** Текущее + В активной ветке + Целевое состояние

**Текущее проверено относительно:** `main` @ `3b7df2c`
**Активная ветка проверена относительно:** `codex/fix-connection-cancel-windows-shell` @ `00d0bcf`
**Дата содержательной проверки:** 2026-08-13

## Как читать статусы

- **Текущее** — находится в `main` и подтверждено production-кодом.
- **В активной ветке** — существует в невлитой ветке и ещё не является частью `main`.
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

Pairing reconciliation использует side-effect-free observation: status polling не подтверждает trust и не создаёт артефакты. Completion выполняется отдельно под durable session/generation lease.

До remote confirmation Mac сохраняет rollback journal и revocation proof. Cleanup имеет отдельные durable стадии для remote revoke, Docker Context и локальных SSH/credential артефактов. Cross-process file locks, generation-scoped leases и schema upgrade gate защищают операции от параллельных процессов Remote Docker и несовместимых writers.

Pairing protocol версионирован. Несовместимые Mac и Windows получают явное требование обновить оба приложения вместо попытки продолжить с различающимся wire-контрактом. Автоматические проверки этого пути пройдены; физический Mac↔Windows результат остаётся обязательным.

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

### В активной ветке

Перед запуском session-owned процессов Mac проверяет согласованность зашифрованной локальной Syncthing identity и owner-scoped ключа из credential store. Низкоуровневый sync-компонент только классифицирует непригодную пару и не меняет config или credentials.

Если Mac ещё не хранит trusted/active device и pending cleanup, application coordinator атомарно очищает только public identity fields, а затем удаляет только принадлежащие приложению Syncthing credentials. Пустые public identity fields являются durable crash boundary: следующий bootstrap создаёт новую согласованную identity, сохраняя workspaces и исходные файлы. Повторный запуск после частичной очистки идемпотентен.

При наличии device state автоматическая ротация запрещена. Runtime не запускает дочерние процессы и публикует `local_sync_identity_corrupt` без private key, Keychain data и внутренних путей. Согласованная замена identity на обеих машинах остаётся отдельным recovery-сценарием.

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

### В активной ветке: ownership и восстановление Windows UI

Повторный ручной запуск `cmd/remote-docker-desktop` сохраняет существующий same-user single-instance/upgrade gate. Если desktop process уже работает, новый процесс вызывает приватный аутентифицированный local API с ограниченным context, требует подтверждённый ответ `shown: true` и завершается до создания agent, lifecycle supervisor или UI child. Ошибка transport/focus, ответ `shown: false` и ещё не готовое приложение не считаются успешным показом и также не создают конкурирующий desktop/agent/UI.

Один desktop application владеет одним точным UI child и его поколением. Для работающего child выполняется focus через существующий приватный endpoint; естественно завершившийся child запускается один раз. Ошибка или timeout focus допускает не более одной ограниченной попытки recovery: launcher повторно проверяет exact command, process handle, completion channel, generation и текущую operation ownership, завершает только этот child и только затем может запустить replacement. Поиск окон или процессов по title, общему имени или PID не используется; foreign или более новый child не изменяется.

`exec.Cmd.ProcessState` не является общей liveness-моделью: launcher использует собственное синхронизированное состояние и exact completion channel. `Stop` терминально закрывает launcher для будущих `Show`, поэтому поздний или уже выполняющийся focus не может запустить UI после Finish work. Одновременные Stop присоединяются к точной stop operation; если точный child ещё жив после ошибки, только один caller может выполнить единственную terminal retry-попытку. Известная ложная ошибка после уже исчерпанного retry вынесена отдельно в backlog и не означает работающий child или повторное завершение процесса.

Закрытие окна крестиком остаётся скрытием в tray, а не Finish work; повторный запуск ярлыка должен показать существующее окно. Эти Windows-сценарии подтверждены только сфокусированными автоматическими тестами и cross-compilation активной ветки. Физическая проверка точного artifact ещё не выполнена.

Изменение не добавляет listener, port, service, autostart, protocol или schema и не меняет границу same-user local API.

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

Pairing generation, revocation proof, rollback stages, cleanup lease и Docker Context owner token входят в текущий config schema. Docker Context изменяется или восстанавливается только при точном совпадении owner token, endpoint и managed description; неоднозначное ownership приводит к fail-closed результату без мутации чужого context.

## Lifecycle операций

### Текущее

- **Start client/hosting** запускает session-owned runtime только после явного действия.
- **Pause** останавливает runtime и возвращает приложение в `paused`, но не удаляет доверие.
- **Disconnect** завершает текущую connection session, сохраняя trusted peer.
- **Forget** удаляет выбранное доверие и принадлежащие ему local artifacts; remote revoke может потребовать доступный Windows peer.
- **Finish work** переводит lifecycle в terminal stopping, завершает runtime, relays, child processes и desktop/UI shell.
- **Close window** не равен Finish work и оставляет tray/menu-bar application доступным.

Приложение не регистрирует autostart. После reboot его запускают вручную.

### В активной ветке: отмена установки соединения

Отмена незавершённого подключения отделена от управления уже установленной session и от удаления доверия:

- **Cancel pairing** отменяет только точную текущую pairing session, если её trust ещё не зафиксирован полностью.
- **Stop connection attempt** останавливает runtime в `pairing` или `connecting`, не забывая уже зафиксированный trusted peer.
- **Disconnect** завершает текущую рабочую session, сохраняя доверие для повторного подключения без нового comparison code.
- **Pause** останавливает принадлежащий session runtime и оставляет приложение открытым в `paused`; доверие сохраняется, кроме случая, когда независимый proof-authenticated exact revoke уже сохранён на Windows.
- **Forget/revoke** остаётся отдельной операцией, которая удаляет durable trust и принадлежащие ему артефакты.

Cancel/stop сначала переводит lifecycle в `stopping`. Pairing session и comparison code очищаются только после успешного завершения принадлежащего приложению runtime и watchdog cleanup. Ошибка остановки возвращает прежнюю фазу `pairing` или `connecting`, сохраняет code/session и ownership незавершённых компонентов, поэтому пользователь может повторить операцию. Полностью зафиксированное доверие не удаляется обычными cancel, stop, disconnect или pause.

Если Windows успел подтвердить trust, а Mac ещё не завершил локальный commit, Mac хранит exact session/generation journal и revocation proof. Успешный ранний revoke не считается окончательным, пока TLS-pinned observe-only запрос точной session не подтвердит, что ранее допущенный Confirm завершился. До этой quiescence boundary proof и journal остаются пригодными для повторной exact-generation очистки после timeout или restart.

На Windows proof-authenticated revoke сначала удаляет принадлежащие pairing артефакты и сохраняет durable registry, а затем вне config transaction и server lock уведомляет lifecycle о точных device/session. Если lifecycle в это время находится в `stopping`, уведомление остаётся привязанным к точному завершённому pairing и доставляется локально после успешного non-terminal `StopCompleted` для cancel, disconnect или pause. Такая доставка не повторяет network revoke, installer cleanup или config save. `StopFailed` и terminal quit не подтверждают уведомление ложно; stale session/generation не очищает более новое trust.

UI передаёт cancel/stop через отдельную local API операцию с ограниченным временем выполнения, показывает loader и блокирует повторный клик до результата. Это подтверждено сфокусированными автоматическими тестами, но ещё не проверено на физической паре Mac↔Windows.

Эта ветка не добавляет protocol/schema version, LAN ports, listeners, services или autostart. Физическая проверка отложена до интеграции отдельных изменений Windows window activation и tray icon и сборки точных artifacts.

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
| Необратимые pairing/cleanup действия имеют явный owner и durable intent | Текущее |
| Status/snapshot не выполняет скрытое completion или revoke | Текущее |
| Cleanup идемпотентен и не изменяет чужие ресурсы | Текущее; требует физической проверки |
| Независимые cleanup stages имеют независимые bounded contexts | Текущее |
| Finish work прекращает всю принадлежащую приложению фоновую работу | Целевое; требует физической проверки |
| Документы не выдают автоматические tests за физическое доказательство | Текущее правило проекта |

## Известные ограничения

- Физическая готовность `main` не подтверждена на полном Mac↔Windows сценарии.
- Только один trusted Mac.
- Только private LAN; Internet mode отсутствует.
- Только TCP publications; UDP и host networking не поддерживаются.
- Workspaces первой версии ограничены Mac paths ниже `/Users`.
- Packages unsigned и требуют checksum/manifest verification.
- Docker CLI не предоставляет absolute atomic compare-and-swap для context относительно произвольного внешнего CLI process; приложение сериализует свои процессы и использует ownership checks с fail-closed policy.
- Влияние VPN/security software и длительные resource leaks требуют физической проверки.
