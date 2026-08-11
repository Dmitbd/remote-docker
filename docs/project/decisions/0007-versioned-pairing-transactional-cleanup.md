# ADR 0007: Версионированный pairing и transactional cleanup

- **Статус:** Принято
- **Дата:** 2026-08-11
- **Проверено относительно:** `main` @ `cfc06ec`

## Контекст

Remote confirmation может установить trust на Windows раньше, чем Mac завершит запись SSH, Keychain, Docker Context и public config. Pause, Quit, потеря сети, crash или второй процесс в этом интервале не должны оставлять скрытое доверие, удалять новое pairing либо изменять чужой Docker Context.

Одновременно обновление только одного устройства не должно незаметно смешивать несовместимые wire и persisted-state контракты.

## Решение или испробованный подход

- Pairing protocol имеет явную версию; несовместимая пара останавливается с upgrade-required результатом.
- До remote confirmation Mac сохраняет durable rollback journal, уникальную generation и revocation proof; Windows хранит только proof hash и public pairing metadata.
- Status polling остаётся observe-only. Completion и cleanup выполняются отдельно под generation-scoped leases и общим cross-process state lock.
- `localOnly` атомарно фиксирует запрет remote revoke. Независимые remote, Docker и local cleanup stages имеют отдельные bounded contexts и повторяются идемпотентно.
- Docker Context получает уникальный owner token. Create/update/restore/remove разрешены только при точном совпадении token, endpoint и managed description; ambiguous ownership завершается без мутации.
- Config schema и desktop startup имеют upgrade gate: новый writer сначала подтверждает остановку несовместимого same-user process, затем выполняет migration под state lock.
- Windows revoke удаляет принадлежащие pairing SSH и Syncthing artifacts до удаления registry record.

## Фактический результат и доказательства

Контракт реализован в `internal/app`, `internal/pairing`, `internal/config`, `internal/dockercli`, `internal/filelock` и desktop startup/packaging paths. Focused race tests, process-recovery tests, packaging contracts и Windows cross-compilation пройдены для integration commit `cfc06ec`.

Физический Mac↔Windows pairing, update и crash recovery остаются обязательными строками `VERIFICATION.md` и не считаются доказанными автоматическими тестами.

## Последствия

- Cleanup переживает restart и не занимает локальный single-pair slot при недоступном Windows.
- Старое и новое приложение не пытаются выполнять несовместимое pairing; пользователь должен обновить оба устройства.
- Неоднозначный Docker Context сохраняется как пользовательское действие вместо автоматического удаления.
- Journal, leases и proof lifecycle увеличивают сложность persisted state и требуют строгого порядка locks.
- Docker CLI не предоставляет atomic CAS относительно произвольного внешнего CLI process; строгая гарантия действует между процессами Remote Docker, а внешние изменения обрабатываются fail-closed checks.

## Почему принят текущий статус

In-memory completion, side-effectful status polling, общий description Docker Context и недоказуемый best-effort rollback создавали достижимые crash/concurrency окна. Durable generation, proof, ownership token и version gate закрывают эти окна без нового внешнего сервиса и сохраняют фиксированный сценарий Mac→Windows.

## Условия повторного рассмотрения

Решение пересматривается, если pairing protocol получит формальную backward-compatible negotiation, Docker предоставит conditional context operations либо продукт перейдёт от одной пары к multi-peer trust model.

## Связанные материалы

- [Pairing architecture](../ARCHITECTURE.md#discovery-и-pairing)
- [Данные, keys и ownership](../ARCHITECTURE.md#данные-keys-и-ownership)
- [Физическая проверка pairing](../VERIFICATION.md#физическая-macwindows-матрица)
- [RD-B001](../BACKLOG.md#rd-b001-проверить-полный-pairing-и-forget-на-реальных-устройствах)
