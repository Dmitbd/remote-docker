# ADR 0003: Синхронизация source в WSL Linux filesystem

- **Статус:** Принято
- **Дата:** 2026-08-06
- **Проверено относительно:** `main` @ `3dc60ed`

## Контекст

Docker bind mount создаётся на host Docker daemon, а не на client machine. Удалённый Engine не может напрямую примонтировать Mac path. Размещение active project под `/mnt/c` также добавляет cross-filesystem overhead и Windows/Linux semantic differences.

## Решение или испробованный подход

Пользователь явно регистрирует Mac source workspaces. Remote Docker синхронизирует их через Syncthing в WSL Linux filesystem и переводит разрешённые bind paths на remote copy.

Прямая попытка использовать Mac path на remote daemon, полный mirror home directory и размещение performance-sensitive source под `/mnt/c` отклонены.

## Фактический результат и доказательства

Workspace resolver, bind analysis, sync readiness и remote typed RPC находятся в `internal/workspace`, `internal/dockercli`, `internal/syncer` и `internal/app/sync_runtime.go`.

## Последствия

- Source имеет Mac copy и WSL copy; readiness обязательна перед bind-dependent Docker command.
- Named volumes, databases, images и build cache не синхронизируются.
- Нужно явно обрабатывать conflicts, ignores, deletion и disk exhaustion.

## Почему принят текущий статус

Без remote copy обычные development bind mounts не выполняют пользовательский сценарий. Подход реализован в `main`.

## Условия повторного рассмотрения

Sync implementation можно заменить после реального benchmark и license audit, если новый механизм сохраняет explicit workspace policy и Docker command compatibility.

## Связанные материалы

- [Workspace flow](../ARCHITECTURE.md#поток-синхронизации-workspace)
- `internal/app/preflight.go`
- `internal/app/sync_runtime.go`
- [RD-B003](../BACKLOG.md#rd-b003-проверить-workspace-synchronization-и-tcp-localhost-relay)
