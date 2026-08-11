# ADR 0004: Authenticated tunnel без открытого Docker API

- **Статус:** Принято
- **Дата:** 2026-08-10
- **Проверено относительно:** `main` @ `3dc60ed`

## Контекст

Docker socket даёт практически полный контроль над host. Открывать Docker TCP API или внутренние WSL SSH/Syncthing services напрямую в Wi-Fi небезопасно и делает lifecycle зависимым от изменяемого WSL IP, portproxy и стороннего network software.

Ранний прямой LAN bridge внутренних ports оказался сложным для ownership, firewall и сетевой диагностики.

## Решение или испробованный подход

Windows публикует один tunnel port `49221`. Pairing и рабочая session используют отдельные TLS ALPN, pinned Ed25519 identities и authenticated peer. Yamux переносит только четыре фиксированных stream kinds; Windows bridge разрешает только WSL SSH `22` и Syncthing `22000`.

Открытый Docker API `2375/2376`, generic proxy и постоянное прямое LAN exposure внутренних WSL services отклонены.

## Фактический результат и доказательства

Transport, typed streams, fixed relays и private target validation реализованы в `internal/tunnel`, `internal/windowsbridge` и `internal/systemtransport`. Firewall contract использует один рабочий tunnel boundary.

## Последствия

- Нужно поддерживать reconnect, admission, mTLS identity и protocol version.
- Mac получает stable loopback endpoints независимо от WSL IP.
- Tunnel нельзя использовать как arbitrary network proxy.
- Реальное взаимодействие с VPN/security software остаётся обязательной проверкой.

## Почему принят текущий статус

Secure tunnel влит в `main` и заменяет прямую публикацию внутренних runtime ports как основной transport.

## Условия повторного рассмотрения

Дополнительный transport возможен только при сохранении authenticated identity, private-network policy, fixed service allowlist и отсутствия открытого Docker API.

## Связанные материалы

- [Рабочий tunnel](../ARCHITECTURE.md#рабочий-tunnel)
- `internal/tunnel`
- `internal/windowsbridge`
- [RD-B006](../BACKLOG.md#rd-b006-проверить-сеть-adguard-и-recovery-permutations)
