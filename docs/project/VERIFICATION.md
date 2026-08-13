# Проверка и release gate

**Статус документа:** Текущее

**Проверено относительно:** `main` @ `b5103e9`
**Дата содержательной проверки:** 2026-08-13

## Правила доказательств

- Unit/integration tests доказывают только проверенный контракт и не заменяют реальную ОС.
- Cross-compilation доказывает компилируемость target, но не запуск.
- Packaging contract доказывает структуру artifact/script, но не успешную установку.
- Физический результат относится только к указанным version, commit, artifact, ОС и окружению.
- Не выполненная проверка остаётся `Не проверено` или `Требует устройства`.
- Ошибка одной строки не превращается автоматически в архитектурный рефакторинг: сначала сохраняются exact evidence и root cause boundary.

## Автоматические проверки

| Область | Существующее доказательство | Статус для `main` |
|---|---|---|
| Lifecycle state machine | `internal/lifecycle/*_test.go` | Реализовано в CI, не физическая проверка |
| Pairing protocol и runtime | `internal/pairing`, `internal/app` tests | Focused race/review пройдены для `b5103e9`; не физическая проверка |
| Tunnel TLS/yamux/reconnect | `internal/tunnel`, `tests/transportlab` | Реализовано в CI; flake см. RD-B009 |
| Docker command analysis/preflight | `internal/dockercli`, `internal/app` tests | Реализовано в CI, не реальный проект |
| Workspace policy/sync control | `internal/workspace`, `internal/syncer`, `internal/app` tests | Реализовано в CI, не две физические файловые системы |
| Port relay ownership | `internal/portrelay`, `internal/sshtransport` tests | Реализовано в CI, не реальный published port |
| Desktop UI model/operations | `internal/desktopui`, `cmd/remote-docker-ui` tests | Focused race/review пройдены для `b5103e9`; физическая Windows activation/recovery не проверена |
| WSL/package contracts | integration scripts и Pester | Windows и macOS package CI пройден для `09ba7ca`; установка требует устройства |

Конкретный PR запускает только относящиеся к изменению packages и contracts согласно `AGENTS.md`.

### В `main`: восстановление Mac Syncthing identity

- `internal/syncer` классифицирует валидную identity, неверный key, повреждённый blob и некорректный key без вывода secret details.
- `internal/app` проверяет автоматический reset только для непривязанного Mac, сохранение workspaces, порядок config-before-credentials, идемпотентность после частичной очистки и fail-closed поведение при trusted/pending state или ошибке credential store.
- `AgentRuntime` не публикует running и не запускает session children до завершения preflight.
- `Supervisor` и `DesktopController` публикуют `local_sync_identity_corrupt` без внутренней причины, если безопасная автоматическая ротация запрещена.
- Сфокусированный race-набор `internal/lifecycle`, `internal/syncer`, `internal/app` пройден локально. Это не является физической проверкой macOS Keychain, Windows Syncthing или pairing.

### В `main`: отмена установки соединения

- `internal/localapi`, `internal/desktopui` и desktop UI command tests подтверждают отдельные cancel/stop операции, ограниченный operation context, loader/disabled guard и освобождение кнопки после ошибки.
- `internal/lifecycle` и `internal/app` race tests подтверждают переход `pairing`/`connecting` → `stopping`, очистку comparison code только после успешного stop, retry после `StopFailed` и сохранение полностью committed trust.
- Mac coordinator race tests подтверждают exact session/generation arbitration, сохранение revocation proof после partial commit и delayed Confirm, TLS-pinned observe-only quiescence fence и защиту более нового pairing от stale cleanup.
- Windows pairing/app race tests и pinned loopback TLS-сценарий подтверждают порядок durable registry revoke → lifecycle notification, сохранение точного уведомления во время `stopping` и его однократную локальную доставку после успешного cancel/disconnect/pause без второго network revoke или config save.
- `internal/assets` test подтверждает явное отображение lifecycle `pairing_cancellation_pending` в существующий tray asset `pairing`; новый tray state или asset не вводится.
- Автоматические проверки не подтверждают реальный WebView, tray/menu-bar, firewall/VPN path, OS process cleanup или обмен между двумя устройствами. Физическая матрица остаётся непройденной до сборки точных artifacts.
- Изменение не добавляет protocol/schema version, LAN ports, listeners, services или autostart.

### В `main`: повторный запуск и bounded recovery Windows UI

- Desktop command tests подтверждают цепочку `second instance → private authenticated local API → Application.Show → exact owned UI`: вызов ограничен context, требует `shown: true`, а error/false/not-ready не выдаются за успех и не продолжают создание второго agent/UI.
- `internal/desktop` race tests подтверждают один launcher-owned child, focus работающего child, однократный запуск после natural exit и не более одного bounded recovery после focus error/timeout.
- Recovery и terminal Stop сверяют exact command, process handle, completion channel, generation и operation ownership; код не перечисляет и не ищет окна/процессы по title, имени или PID и не завершает foreign/newer child.
- Launcher-owned synchronized state заменяет чтение `exec.Cmd.ProcessState`; исходно воспроизведённая race закрыта regression tests текущего `main`.
- Stop терминален для launcher. Поздний/in-flight Show не запускает UI после Finish work; concurrent Stop присоединяется к exact operation, а retry arbitration разрешает не более одной terminal retry-попытки.
- Сфокусированный race-набор `internal/desktop` и desktop command, а также Windows cross-compilation обоих test binaries пройдены локально для `00d0bcf`. Это доказывает проверяемые контракты и компилируемость target, но не поведение Windows, WebView2, shortcut, tray или реальных процессов.
- Listener, port, service, autostart, protocol и schema не добавлялись.

### В `main`: state-specific Windows tray icon

- Детерминированный generator и его tests подтверждают пять Windows ICO (`paused`, `search`, `pairing`, `connected`, `error`), PNG-backed entries 16/20/24/32 px и сохранение отдельного полноразмерного application ICO.
- `internal/assets` tests подтверждают exact embedded ICO bytes для Windows, существующие template PNG для macOS и цветной regular PNG fallback для Unix/unknown platforms.
- `internal/desktop` tests подтверждают выбор state asset и mode: Windows ICO/regular, macOS PNG/template, Unix/unknown PNG/regular.
- Windows tray ICO входят в `RemoteDocker.exe` через Go `embed` и не устанавливаются отдельными файлами. NSIS продолжает копировать отдельный `remote-docker.ico` для application/shortcut; существующий package contract проверяет этот путь и отсутствие autostart.
- Сфокусированные Go tests и Windows cross-compilation подтверждают source/build contract, но не видимость или качество рендеринга icon в Windows shell. Реальный installer для `main` @ `b5103e9` не собирался.
- Listener, port, service, autostart, protocol и schema не добавлялись.

### Интеграционная проверка 2026-08-11

- Focused race tests пройдены для pairing/runtime/config/Docker ownership/file lock/lifecycle/local API/desktop UI и затронутых commands.
- Новый `process-wait` и `single-instance` путь пройден отдельно с race detector. На этом историческом gate общий `ProcessLauncher` race ещё оставался; исправление и его автоматическое доказательство теперь входят в текущий `main` @ `b5103e9`, описанный выше.
- Windows cross-compilation пройдена для `internal/app`, `internal/desktop` и desktop command.
- `internal/provision` contracts проверяют shutdown exit code, ожидание процесса и сохранение rollback binary.
- GitHub Actions run `31497596190` успешно собрал development candidate `0.2.8`: WSL rootfs, unsigned NSIS EXE и unsigned macOS PKG.
- Windows CI прошёл provisioning contract, PowerShell 5.1 compatibility, Pester package contract, NSIS build и checksum verification.
- macOS CI прошёл package contract, сборку PKG, проверку версии `0.2.8 (72)` и checksum verification.
- Скачанные EXE и PKG локально повторно сверены с опубликованными SHA-256 manifests.
- Физический запуск Windows/macOS и сценарий Mac↔Windows этим результатом не подтверждены.

## Packaging checks

| Проверка | macOS | Windows |
|---|---|---|
| Artifact layout и bundled versions | CI `31497596190`: пройдено | CI `31497596190`: пройдено |
| Checksum, manifest, source commit | CI + локальная повторная сверка: пройдено | CI + локальная повторная сверка: пройдено |
| Fresh install UI | Требует устройства | Требует устройства |
| Update с работающего предыдущего релиза | Требует устройства | Требует устройства |
| Uninstall без удаления Docker data | Требует устройства | Требует устройства |
| Explicit permanent data removal | Не применимо к Windows WSL data | Требует устройства |
| Отсутствие autostart после reboot/login | Требует устройства | Требует устройства |

## Физическая Mac↔Windows матрица

Начальный статус всех строк — **Не проверено** для development artifact `0.2.8`, созданного из `main` @ `09ba7ca`.

| Сценарий | Ожидаемый результат | Статус |
|---|---|---|
| Manual start обоих приложений | Оба открываются один раз и начинают в Paused | Не проверено |
| Windows Start hosting + Mac Search | Ожидаемый host появляется без stale entries | Не проверено |
| Pairing approve | Одинаковый code на двух UI; оба переходят в Connected | Не проверено |
| Cancel pairing до approve | Оба UI завершают только точную текущую session; code исчезает после успешной остановки | Не проверено |
| Pairing expiry | Просроченная session завершается на обоих UI и не создаёт доверие | Не проверено |
| Cancel после approve во время connecting | Runtime останавливается; полностью committed trust сохраняется | Не проверено |
| Stop failure и retry | Ошибка не маскируется как завершение; code/session и незавершённый ownership сохраняются; повтор успешен | Не проверено |
| Disconnect и reconnect trusted peer | Trust сохраняется; повторный code не требуется | Не проверено |
| Pause во время authenticated revoke | После успешной остановки Windows остаётся Paused без stale trust; durable revoke не повторяется | Не проверено |
| Disconnect во время authenticated revoke | После успешной остановки Windows ожидает подключения без stale trust; durable revoke не повторяется | Не проверено |
| Stale session/generation cleanup | Старый cancel/revoke не изменяет новый pairing или доверие | Не проверено |
| Forget с доступным Windows | Local и remote trust очищены | Не проверено |
| Forget при недоступном Windows | Local slot освобождён; cleanup не блокирует новое pairing | Не проверено |
| Второй Mac | Busy не нарушает первую session | Требует устройства |
| Docker basic operations | Engine на Windows, команды с Mac успешны | Не проверено |
| Compose project | Build, binds, volumes, networks и exec работают | Не проверено |
| Workspace changes | WSL copy становится ready до Docker command | Не проверено |
| Recovery повреждённой Mac Syncthing identity | Unpaired Mac сохраняет workspaces, создаёт новую identity и завершает pairing/sync | Не проверено |
| Повреждённая identity при сохранённом device state | Ничего не ротируется; UI показывает безопасное действие | Не проверено |
| Published TCP port | Тот же Mac localhost port доступен | Не проверено |
| Foreign local port conflict | Понятная ошибка; чужой process остаётся | Не проверено |
| Pause | Runtime останавливается, приложение остаётся открытым | Не проверено |
| Finish work | UI закрывается, app-owned background work отсутствует | Не проверено |

### Физическая Windows desktop boundary текущего `main`

Для `main` @ `b5103e9` installer/artifact не собирался. Все строки ниже остаются **Не проверено** до запуска точного artifact на Windows.

| Сценарий | Ожидаемый результат | Статус |
|---|---|---|
| Закрыть окно крестиком | В текущей конфигурации Wails завершается только UI child; desktop process, lifecycle/runtime и tray остаются, Finish work не выполняется | Не проверено |
| Повторно запустить ярлык после X | Второй launcher завершается; `Show` создаёт replacement UI child, а не показывает скрытое прежнее окно | Не проверено |
| Проверить recreation UI после X | Новый UI child получает fresh transient UI state; desktop/lifecycle/runtime и tray не перезапускаются | Не проверено |
| Сверить process tree после повторных запусков | Работают ровно один desktop process и один принадлежащий ему UI child | Не проверено |
| Focus error или неотвечающий exact UI | Одна bounded recovery-попытка завершает только exact owned child и запускает не более одного replacement | Не проверено |
| Ошибка recovery | Операция завершается ограниченно с ошибкой; foreign/newer process не изменяется; retry loop отсутствует | Не проверено |
| Finish work одновременно с focus/Stop | После завершения нет owned UI child; поздний или in-flight Show не запускает новый | Не проверено |
| Tray: `paused`, светлая и тёмная taskbar | Значок видим и соответствует paused-state | Не проверено |
| Tray: `search`, светлая и тёмная taskbar | Значок видим и отличается от paused-state | Не проверено |
| Tray: `pairing`, светлая и тёмная taskbar | Значок видим и отличается от search-state | Не проверено |
| Tray: `connected`, светлая и тёмная taskbar | Значок видим и соответствует connected-state | Не проверено |
| Tray: `error`, светлая и тёмная taskbar | Значок видим и явно отличается от normal states | Не проверено |
| Application и shortcut icon | Ярлык и executable используют отдельный полноразмерный application icon, а не state-specific tray ICO | Не проверено |
| Крестик и tray icon | Обычный X завершает только UI child; state icon остаётся доступен возле часов | Не проверено |
| Finish work и tray icon | После terminal shutdown значок исчезает и desktop/UI processes завершены | Не проверено |

## Сеть и recovery

Проверяются отдельные комбинации:

| Mac AdGuard | Windows AdGuard | Pair/connect | Reconnect после Wi-Fi loss | Статус |
|---|---|---|---|---|
| Off | Off | ожидается успех | ожидается успех | Не проверено |
| On | Off | ожидается успех | ожидается успех | Не проверено |
| Off | On | ожидается успех | ожидается успех | Не проверено |
| On | On | ожидается успех | ожидается успех | Не проверено |

Дополнительно:

- sleep/wake Mac;
- sleep/wake Windows;
- смена Windows WSL IP;
- restart Windows без autostart;
- private/public Windows network profile;
- краткий и длительный Wi-Fi разрыв;
- повторное появление host после manual launch.

Каждый failure фиксируется с timestamp, UI state на обеих сторонах, listener/connection ownership и границей: Mac transport, Windows tunnel, WSL service или application protocol.

## Процессы, CPU, RAM и утечки

Проверка выполняется в состояниях:

1. приложение не запущено;
2. Paused;
3. Searching/Host waiting;
4. Connected idle;
5. Docker build/active containers;
6. после Disconnect;
7. после Finish work.

Фиксируются только app-owned показатели:

- desktop/UI/watchdog child processes;
- Syncthing и SSH forward children;
- fixed loopback listeners и tunnel connection;
- managed WSL service/workload state;
- process CPU/RAM, handles/file descriptors и connection count;
- trend памяти и количества ресурсов во времени.

Критерий утечки — воспроизводимый неограниченный рост при повторении одинаковой операции или длительном стабильном состоянии, а не единичный пик/кэш без пользовательского влияния.

## Install, update, uninstall

### Windows

- visible wizard без пустых component pages;
- один понятный выбор application/data roots;
- WSL prerequisites и restart имеют явный результат;
- update останавливает старый process и проверяет его исчезновение до замены files;
- никакие consoles не остаются после завершения;
- normal uninstall сохраняет managed data;
- explicit destructive removal требует точного подтверждения и не принимает широкие/unowned paths;
- owned firewall rules удаляются, чужие rules сохраняются.

### macOS

- package version совпадает с manifest;
- устанавливается одна desktop application;
- unrelated `/usr/local/bin/docker` не перезаписывается;
- launcher/context ownership проверяется перед cleanup;
- после Finish work отсутствует app-owned background work;
- autostart не создаётся.

## Release gate

Release artifact не считается стабильным, пока:

- нет открытых подтверждённых P0;
- нет открытых P1-дефектов основного сценария;
- RD-B001–RD-B006 имеют успешный физический результат для release candidate;
- относящиеся automated/package checks пройдены на release commit;
- `PRODUCT.md` и `ARCHITECTURE.md` сверены с release commit;
- update/uninstall проверены с предыдущей поддерживаемой версией;
- checksums, manifest, SBOM и source commit опубликованы вместе с unsigned packages;
- все отложенные P2/P3 перечислены без маскировки как выполненные.

## Журнал физических прогонов

Запись добавляется только после реального прогона:

```text
Дата:
Commit/tag:
Artifacts:
macOS + hardware:
Windows + hardware:
Network/security software:
Проверенные строки:
Результат:
Evidence summary:
Открытые backlog IDs:
```

На 2026-08-11 физического прогона development candidate `0.2.8` из `main` @ `09ba7ca` нет.
