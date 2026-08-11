# ADR 0002: Managed WSL2 с ordinary Docker Engine

- **Статус:** Принято
- **Дата:** 2026-08-06
- **Проверено относительно:** `main` @ `cfc06ec`

## Контекст

Windows должен выполнять Linux container workload без переноса Docker runtime на Mac. Зависимость от Docker Desktop возвращает отдельную тяжёлую desktop VM, лицензионные и lifecycle ограничения и не даёт приложению полного ownership управляемого окружения.

## Решение или испробованный подход

Remote Docker создаёт отдельную managed WSL2 distribution с ordinary Docker Engine, OpenSSH, Syncthing и remote agent. Docker data хранится внутри этого окружения.

Обязательная зависимость от Docker Desktop и поддержка Windows containers отклонены для MVP.

## Фактический результат и доказательства

WSL rootfs, provisioning scripts, systemd units и package contracts находятся в `packaging/wsl` и `packaging/windows`. README и installer contract прямо указывают, что Docker Desktop не требуется.

## Последствия

- Приложение отвечает за provisioning, update и safe uninstall managed environment.
- Требуются WSL2, virtualization и Windows-specific physical tests.
- Docker images, volumes и cache не зависят от Mac lifecycle.

## Почему принят текущий статус

Это базовая runtime architecture текущего `main` и единственный поддерживаемый Windows Engine path.

## Условия повторного рассмотрения

Возможен дополнительный backend после стабильного MVP, если готовый OSS runtime снижает installer/maintenance cost без изменения Docker-only workflow и security boundary.

## Связанные материалы

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- `packaging/wsl`
- `packaging/windows`
- [RD-B011](../BACKLOG.md#rd-b011-сравнить-готовые-oss-компоненты-с-текущей-инфраструктурой)
