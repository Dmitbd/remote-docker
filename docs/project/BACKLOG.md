# Структурированный backlog

**Статус документа:** Текущее

**Текущее проверено относительно:** `main` @ `3b7df2c`
**Активная ветка проверена относительно:** `codex/fix-connection-cancel-windows-shell` @ `0993ff80b19614ed2596deb32f5ceb44dd50da6f`
**Дата содержательной проверки:** 2026-08-13

Backlog содержит только незавершённую работу. Наличие пункта не разрешает агенту автоматически брать его в текущую задачу.

## Классификация

### Тип

- **Дефект** — наблюдаемое или доказанное неправильное поведение.
- **Риск** — достижимая опасность, результат которой ещё не воспроизведён полностью.
- **Проверка** — обязательное доказательство готовности.
- **Исследование** — ограниченный эксперимент перед архитектурным решением.
- **Улучшение** — полезное поведение вне обязательного MVP.

### Приоритет

- **P0** — подтверждённая уязвимость, потеря данных, повреждение ОС или раскрытие Docker/keys.
- **P1** — сломан основной MVP либо приложение оставляет процессы, доверие или ресурсы в некорректном состоянии.
- **P2** — важная проблема с обходным путём, редкая аварийная гонка или ограничение вне основного сценария.
- **P3** — удобство, оптимизация или возможность после MVP.

### Доказательство

- **Гипотеза** — есть основание проверить, но нет достаточного доказательства.
- **Подтверждено кодом** — существует конкретный исполняемый путь.
- **Воспроизведено** — проблема или результат повторены фактически.
- **Требует устройства** — честная проверка невозможна без реальной пары устройств.
- **Опровергнуто** — проверка показала отсутствие предполагаемой проблемы; такой пункт удаляется из активного backlog после фиксации результата.

## Блокирует рабочий MVP

### RD-B001: Проверить полный pairing и forget на реальных устройствах

- **Тип:** Проверка
- **Приоритет:** P1
- **Доказательство:** Требует устройства
- **Влияние:** Без подтверждённого pairing пользователь не может начать основной Docker workflow; stale trust ломает повторное подключение.
- **Известно:** Пользователь ранее наблюдал отсутствие Windows approval UI, непринятый code и stale forgotten device. Исправления pairing reconciliation и restart-safe cleanup влиты в `main`. В активной ветке автоматические race/TLS-сценарии дополнительно покрывают точную отмену pairing/connecting, retry после stop failure, сохранение committed trust, Mac exact-generation cleanup и Windows durable-revoke notification. Эти тесты не подтверждают поведение реальных UI, сети и ОС.
- **Критерий завершения:** Search → select → code on both devices → approve/cancel/expiry → connected → disconnect → forget → repeat pairing работает без restart UI и без stale state. Отдельно подтверждены cancel до approve, cancel после approve во время connecting, stop failure с успешным retry, pause/disconnect одновременно с authenticated revoke и защита нового pairing от stale session/generation cleanup.
- **Проверка:** Реальные Mac и Windows, fresh config и upgrade config; точные artifacts собираются после интеграции Windows window activation и tray icon, а результаты обоих UI фиксируются в `VERIFICATION.md`.
- **Связано:** [MVP](PRODUCT.md), [pairing architecture](ARCHITECTURE.md#discovery-и-pairing).

### RD-B002: Проверить обычную Docker/Compose совместимость с Mac

- **Тип:** Проверка
- **Приоритет:** P1
- **Доказательство:** Требует устройства
- **Влияние:** Продукт не достигает цели, если wrapper работает только с частью Docker-команд.
- **Известно:** Unit/e2e contracts покрывают анализ команд, но не заменяют реальный Windows Engine и настоящий проект.
- **Критерий завершения:** `docker info`, `ps`, `build`, `run`, `exec`, `logs`, `cp`, `compose up/down/build/exec/logs` выполняются с Mac; Engine и data находятся на Windows.
- **Проверка:** Реальный проект с build contexts, bind mounts, named volumes и несколькими Compose services.
- **Связано:** [Docker command flow](ARCHITECTURE.md#поток-docker-команды), `tests/e2e/docker_compatibility.sh`.

### RD-B003: Проверить workspace synchronization и TCP localhost relay

- **Тип:** Проверка
- **Приоритет:** P1
- **Доказательство:** Требует устройства
- **Влияние:** Bind mounts могут получить устаревший source, а опубликованный сервис — быть недоступным с Mac.
- **Известно:** В коде есть Syncthing readiness и SSH port supervisor; полного физического результата для release artifact нет. В активной ветке добавлено безопасное восстановление несовместимой локальной Syncthing identity для непривязанного Mac; автоматические проверки не доказывают работу с реальным Keychain и двумя ОС.
- **Критерий завершения:** Изменения Mac source достигают WSL до Docker execution; conflict/error виден; поддерживаемый TCP port доступен на том же свободном Mac localhost port; чужой local listener не завершается. Кандидат поверх воспроизведённой повреждённой identity восстанавливается без потери workspaces и завершает pairing/sync.
- **Проверка:** Малые и крупные файлы, rename/delete, rapid edits, Compose bind mount, port conflict и removal container publication; отдельно — update Mac-кандидата поверх повреждённой identity, повторное pairing и базовая Docker-команда.
- **Связано:** [workspace flow](ARCHITECTURE.md#поток-синхронизации-workspace), [TCP flow](ARCHITECTURE.md#поток-опубликованного-tcp-порта).

### RD-B004: Проверить чистую установку, update, reboot и uninstall Windows

- **Тип:** Проверка
- **Приоритет:** P1
- **Доказательство:** Требует устройства
- **Влияние:** Ошибка установки или update может оставить старый process, некорректный WSL state либо затронуть чужие OS resources.
- **Известно:** Packaging contracts и проверяемый shutdown/rollback gate находятся в `main`. В активной ветке добавлены exact-window recovery и пять embedded Windows tray ICO; focused tests и cross-compilation не подтверждают их отображение Windows shell. Реальный NSIS/PowerShell/WSL lifecycle после `cfc06ec` не проверен.
- **Критерий завершения:** Fresh install, same-version retry, supported update, reboot continuation, normal uninstall и explicit data removal дают заявленный результат без видимых зависших consoles и autostart. В текущей конфигурации Wails X завершает только UI child; desktop process, lifecycle/runtime и tray остаются, а следующий запуск ярлыка создаёт replacement UI child без второго desktop process. В tray различимы `paused`, `search`, `pairing`, `connected` и `error` на светлой и тёмной taskbar; полноразмерная иконка ярлыка не заменена state icon.
- **Проверка:** Windows installer UI, logs, process list, WSL distributions, firewall rules, app/data directories и preserved Docker data. Отдельно зафиксировать process tree после X и нескольких повторных запусков ярлыка, доказать создание нового UI child и reset transient UI state, а также screenshot каждого tray state на light/dark taskbar и исчезновение icon после **Finish work**.
- **Связано:** `docs/INSTALL.md`, `tests/integration/windows_package.Tests.ps1`, [verification](VERIFICATION.md#install-update-uninstall).

### RD-B005: Проверить cleanup, idle resources и отсутствие утечек

- **Тип:** Проверка
- **Приоритет:** P1
- **Доказательство:** Требует устройства
- **Влияние:** Приложение может продолжать потреблять ресурсы, держать ports/connections или влиять на ОС после Pause/Finish work.
- **Известно:** Supervisor и cleanup paths имеют unit coverage; реального длительного наблюдения на обеих ОС нет.
- **Критерий завершения:** После Pause/Finish work отсутствуют app-owned tunnel, Syncthing, SSH forwarding, watchdog и managed workload processes; фиксированные ports освобождены; CPU idle; память/handles/connections не растут без границы в длительной сессии. Ошибка или timeout focus восстанавливает только exact owned UI одной bounded попыткой; неуспешный recovery не затрагивает foreign/newer process. Finish work одновременно с focus/Stop не оставляет UI child и не допускает поздний relaunch.
- **Проверка:** Process/port snapshots до запуска, connected, paused и finished; длительная idle/active сессия с периодическими samples. На Windows отдельно вызвать успешный и неуспешный exact-UI recovery и гонку Finish work с focus/Stop, сверяя process ownership до и после.
- **Связано:** [lifecycle](ARCHITECTURE.md#lifecycle-операций), [resource verification](VERIFICATION.md#процессы-cpu-ram-и-утечки).

### RD-B006: Проверить сеть, AdGuard и recovery permutations

- **Тип:** Проверка
- **Приоритет:** P1
- **Доказательство:** Требует устройства
- **Влияние:** Пользователь должен работать независимо от того, включён или выключен AdGuard на каждом устройстве, пока private LAN разрешён системой.
- **Известно:** Tunnel ограничен private IP и использует system transport; предыдущая диагностика показала чувствительность сетевого пути, но не установила единый дефект AdGuard.
- **Критерий завершения:** Pairing/connection/reconnect проверены для on/off permutations; Wi-Fi loss, adapter change, sleep/wake и host restart приводят к ограниченному ожиданию и понятному UI state без повторного trust при временном разрыве.
- **Проверка:** Физическая network matrix с timestamps и process/connection snapshots.
- **Связано:** [security boundary](ARCHITECTURE.md#security-boundaries), [network verification](VERIFICATION.md#сеть-и-recovery).

## Важно, но не блокирует текущую физическую проверку

### RD-B008: Интегрировать устранение race состояния UI subprocess

- **Тип:** Дефект
- **Приоритет:** P2
- **Доказательство:** Подтверждено кодом в `main`; исправление подтверждено автоматическими тестами активной ветки
- **Влияние:** В `main` параллельные `Show` и `Wait` могут обращаться к `exec.Cmd.ProcessState` без общей синхронизации. В активной ветке launcher-owned synchronized state устраняет это чтение и одновременно защищает exact-child focus, recovery и terminal Stop.
- **Известно:** Исходная race воспроизведена race detector. В `codex/fix-connection-cancel-windows-shell` она закрыта детерминированными regression tests; focused race gate и Windows cross-compilation пройдены. Ветка ещё не влита, а физические Windows-сценарии вынесены в RD-B004/RD-B005.
- **Критерий завершения:** Исправление с launcher-owned state и exact operation ownership влито в `main`; относящийся focused race gate пройден на merge commit. После этого пункт удаляется из активного backlog, не дожидаясь физической проверки отдельного UI UX.
- **Проверка:** `go test -race -count=1 ./internal/desktop ./cmd/remote-docker-desktop` на merge commit.
- **Связано:** `internal/desktop/uiprocess.go`, [Windows UI ownership](ARCHITECTURE.md#в-активной-ветке-ownership-и-восстановление-windows-ui).

### RD-B009: Подтвердить или опровергнуть transport-lab flake

- **Тип:** Риск
- **Приоритет:** P2
- **Доказательство:** Гипотеза
- **Влияние:** Реальная нестабильность reconnect/tunnel test может скрывать timeout race либо быть только scheduling noise.
- **Известно:** Исторически наблюдался один неуспешный повтор из десяти, после чего полный прогон прошёл. Достаточного воспроизведения нет.
- **Критерий завершения:** Ограниченный повтор либо воспроизводит один и тот же failure с диагностикой, либо не воспроизводит его в заранее заданном числе запусков и пункт закрывается без изменения production-кода.
- **Проверка:** Отдельная diagnostic-задача с сохранением exact failing test/output; не выполнять в рамках документационных или pairing fixes.
- **Связано:** `tests/transportlab`.

### RD-B010: Проверить busy-сценарий второго Mac

- **Тип:** Проверка
- **Приоритет:** P2
- **Доказательство:** Требует устройства
- **Влияние:** Второй клиент должен получить понятный busy, не нарушая рабочую session первого.
- **Известно:** Transport имеет authenticated admission status и one-session server; физический сценарий не выполнялся.
- **Критерий завершения:** Первый Mac продолжает Docker operations; второй видит busy; после освобождения host второй подключается и busy state очищается.
- **Проверка:** Windows и два Mac либо эквивалентная физическая client pair.
- **Связано:** [connection limit](ARCHITECTURE.md#системная-граница).

### RD-B012: Убрать ложную ошибку после исчерпанного terminal retry UI

- **Тип:** Дефект
- **Приоритет:** P3
- **Доказательство:** Подтверждено кодом
- **Влияние:** Если первая terminal stop-попытка и разрешённая единственная retry-попытка завершились ошибкой, exact UI child затем вышел естественно, а поздний внутренний caller снова вызвал Stop, он может получить сохранённую ошибку retry. Последствие ограничено ложным error result: живого child уже нет, повторного signal/kill, retry loop и зависания Finish work этот путь не создаёт.
- **Известно:** Exact stop operation намеренно сохраняется для join/retry arbitration. После исчерпания retry поздний natural exit не заменяет сохранённый result. Независимое review классифицировало сценарий как Minor; он не блокирует физическую проверку или MVP.
- **Критерий завершения:** Детерминированный тест подтверждает сценарий, после чего поздний Stop для уже завершившегося exact child возвращает success без нового signal/kill и без разрешения дополнительного retry.
- **Проверка:** Отдельная узкая задача и focused race test `internal/desktop`; production-код не менять в документационной задаче.
- **Связано:** `internal/desktop/uiprocess.go`, [Windows UI ownership](ARCHITECTURE.md#в-активной-ветке-ownership-и-восстановление-windows-ui).

## После стабилизации MVP

### RD-B011: Сравнить готовые OSS-компоненты с текущей инфраструктурой

- **Тип:** Исследование
- **Приоритет:** P3
- **Доказательство:** Гипотеза
- **Влияние:** Container Desktop или MIT-only Mutagen могут уменьшить собственный Windows provisioning или sync/forwarding code, но смена основы сейчас может задержать рабочий MVP.
- **Известно:** Готового OSS-продукта, полностью совпадающего с fixed Mac→Windows Docker-only workflow, не найдено.
- **Критерий завершения:** Изолированный prototype сравнивает текущую реализацию, Container Desktop backend и MIT-only Mutagen по Docker compatibility, bind synchronization, ports, lifecycle, license и installer complexity.
- **Проверка:** Отдельная ветка и реальный проект; отсутствие выигрыша закрывает исследование без миграции.
- **Связано:** [ADR index](decisions/README.md).
