# Проверка и release gate

**Статус документа:** Текущее

**Проверено относительно:** `main` @ `3dc60ed`, активная ветка @ `fd6a26f`
**Дата содержательной проверки:** 2026-08-11

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
| Pairing protocol и runtime | `internal/pairing`, `internal/app` tests | Реализовано в CI, активная ветка требует focused review |
| Tunnel TLS/yamux/reconnect | `internal/tunnel`, `tests/transportlab` | Реализовано в CI; flake см. RD-B009 |
| Docker command analysis/preflight | `internal/dockercli`, `internal/app` tests | Реализовано в CI, не реальный проект |
| Workspace policy/sync control | `internal/workspace`, `internal/syncer`, `internal/app` tests | Реализовано в CI, не две физические файловые системы |
| Port relay ownership | `internal/portrelay`, `internal/sshtransport` tests | Реализовано в CI, не реальный published port |
| Desktop UI model/operations | `internal/desktopui`, `cmd/remote-docker-ui` tests | Реализовано в CI; process race см. RD-B008 |
| WSL/package contracts | integration scripts и Pester | Частично platform-specific |

Конкретный PR запускает только относящиеся к изменению packages и contracts согласно `AGENTS.md`.

## Packaging checks

| Проверка | macOS | Windows |
|---|---|---|
| Artifact layout и bundled versions | Автоматический contract существует | Pester/installer contracts существуют |
| Checksum, manifest, source commit | Workflow contracts существуют | Workflow contracts существуют |
| Fresh install UI | Требует устройства | Требует устройства |
| Update с работающего предыдущего релиза | Требует устройства | Требует устройства |
| Uninstall без удаления Docker data | Требует устройства | Требует устройства |
| Explicit permanent data removal | Не применимо к Windows WSL data | Требует устройства |
| Отсутствие autostart после reboot/login | Требует устройства | Требует устройства |

## Физическая Mac↔Windows матрица

Начальный статус всех строк — **Не проверено** для release artifact, созданного после активной pairing fix-ветки.

| Сценарий | Ожидаемый результат | Статус |
|---|---|---|
| Manual start обоих приложений | Оба открываются один раз и начинают в Paused | Не проверено |
| Windows Start hosting + Mac Search | Ожидаемый host появляется без stale entries | Не проверено |
| Pairing approve | Одинаковый code на двух UI; оба переходят в Connected | Не проверено |
| Pairing reject/cancel/expiry | Оба UI получают конечное понятное состояние | Не проверено |
| Disconnect и reconnect trusted peer | Trust сохраняется; повторный code не требуется | Не проверено |
| Forget с доступным Windows | Local и remote trust очищены | Не проверено |
| Forget при недоступном Windows | Local slot освобождён; cleanup не блокирует новое pairing | Не проверено |
| Второй Mac | Busy не нарушает первую session | Требует устройства |
| Docker basic operations | Engine на Windows, команды с Mac успешны | Не проверено |
| Compose project | Build, binds, volumes, networks и exec работают | Не проверено |
| Workspace changes | WSL copy становится ready до Docker command | Не проверено |
| Published TCP port | Тот же Mac localhost port доступен | Не проверено |
| Foreign local port conflict | Понятная ошибка; чужой process остаётся | Не проверено |
| Pause | Runtime останавливается, приложение остаётся открытым | Не проверено |
| Finish work | UI закрывается, app-owned background work отсутствует | Не проверено |

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

На 2026-08-11 физического прогона release candidate после `fix/desktop-pairing-state` нет.
