# ADR 0005: Manual startup и explicit Finish work

- **Статус:** Принято
- **Дата:** 2026-08-09
- **Проверено относительно:** `main` @ `3dc60ed`

## Контекст

Remote Docker нужен только во время выбранной Docker-сессии. Autostart после login/reboot расходует ресурсы, создаёт неожиданные WSL/process side effects и затрудняет понимание, работает ли host.

Одновременно обычное закрытие окна menu-bar/tray application не всегда означает намерение полностью завершить работу.

## Решение или испробованный подход

Приложение не добавляется в autostart. Пользователь вручную запускает обе стороны и явно включает client/hosting role. Close window оставляет tray/menu-bar shell, а Finish work выполняет terminal shutdown принадлежащей приложению работы.

Autostart и silent restart tasks отклонены.

## Фактический результат и доказательства

Lifecycle различает Pause и terminal Quit. Installer/user docs запрещают autostart и требуют manual continuation после reboot.

## Последствия

- После Windows reboot пользователь запускает host вручную.
- UI обязан ясно различать Close, Pause и Finish work.
- Installer/updater должны безопасно остановить работающий process перед replacement.

## Почему принят текущий статус

Это явный пользовательский контракт и текущая lifecycle architecture.

## Условия повторного рассмотрения

Только как отдельная opt-in функция после измерения idle resources и с явным UI/state ownership; default остаётся manual.

## Связанные материалы

- [Lifecycle](../ARCHITECTURE.md#lifecycle-операций)
- `internal/lifecycle`
- `cmd/remote-docker-desktop`
- [RD-B005](../BACKLOG.md#rd-b005-проверить-cleanup-idle-resources-и-отсутствие-утечек)
