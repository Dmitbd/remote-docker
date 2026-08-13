# ADR 0005: Manual startup и explicit Finish work

- **Статус:** Принято
- **Дата:** 2026-08-09
- **Проверено относительно:** `main` @ `b5103e9`

## Контекст

Remote Docker нужен только во время выбранной Docker-сессии. Autostart после login/reboot расходует ресурсы, создаёт неожиданные WSL/process side effects и затрудняет понимание, работает ли host.

Одновременно обычное закрытие окна menu-bar/tray application не всегда означает намерение полностью завершить работу.

## Решение или испробованный подход

Приложение не добавляется в autostart. Пользователь вручную запускает обе стороны и явно включает client/hosting role. Finish work выполняет terminal shutdown принадлежащей приложению работы.

Autostart и silent restart tasks отклонены.

### Интегрированное дополнение: ownership desktop и UI child

Desktop application владеет ровно одним exact UI child: command, process handle, completion channel и generation образуют его ownership. Повторный запуск использует same-user private local API и считается успешным только после подтверждённого `shown: true`; до этого второй launcher не создаёт agent, lifecycle supervisor или UI child.

Для живого exact child `Show` выполняет private focus. Ошибка или timeout допускает только одну bounded recovery: launcher снова сверяет exact command/process/done/generation и operation ownership, завершает только этот child и создаёт не более одного replacement. Поиск или завершение окон/processes по name, title или PID и безграничный relaunch отклонены, потому что не доказывают ownership и могут затронуть foreign/newer process.

Первый `Stop` терминально закрывает launcher для последующих `Show`; in-flight focus не может вернуть UI после Finish work. В текущей конфигурации Wails обычный X завершает только UI child. Desktop process, lifecycle/runtime и tray/menu-bar остаются; следующий `Show` создаёт новый UI child, а не скрывает или повторно показывает то же окно, поэтому transient UI state сбрасывается. X не является Finish work.

## Фактический результат и доказательства

Lifecycle различает Pause и terminal Quit. Installer/user docs запрещают autostart и требуют manual continuation после reboot.

Дополнение подтверждено focused desktop/command tests и Windows cross-compilation; физический Windows shortcut, Wails/WebView2, tray и process-lifecycle result ещё не проверены точным artifact.

## Последствия

- После Windows reboot пользователь запускает host вручную.
- UI обязан ясно различать X (завершает только UI child), Pause и Finish work.
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
