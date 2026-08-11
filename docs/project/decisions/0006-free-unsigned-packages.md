# ADR 0006: Бесплатные unsigned packages

- **Статус:** Принято
- **Дата:** 2026-08-09
- **Проверено относительно:** `main` @ `3dc60ed`

## Контекст

Платные Apple Developer ID и Windows Authenticode certificates добавляют постоянную стоимость. Цель проекта — оставаться бесплатным для сборки и распространения.

Unsigned packages вызывают Gatekeeper/SmartScreen warnings, поэтому происхождение artifact должно проверяться отдельно и нельзя предлагать глобальное отключение защиты ОС.

## Решение или испробованный подход

Release packages остаются unsigned и публикуются вместе с SHA-256 checksums, source commit manifest и SBOM. Пользователь продолжает установку только после проверки artifact. Документация запрещает глобально отключать Gatekeeper, SmartScreen, antivirus или Smart App Control.

Зависимость обязательного релиза от платных signing certificates отклонена.

## Фактический результат и доказательства

README/INSTALL описывают verification flow. Release workflow и integration contracts создают checksums, manifests и SBOM metadata.

## Последствия

- Установка требует дополнительного осознанного шага.
- Некоторые Windows policies могут полностью блокировать unsigned software.
- Artifact provenance и понятные инструкции становятся частью release gate.

## Почему принят текущий статус

Решение соответствует бесплатной модели продукта и реализовано в packaging/release contracts.

## Условия повторного рассмотрения

Подпись можно добавить как необязательный release enhancement при появлении бесплатного устойчивого способа или отдельного финансирования, не удаляя reproducible checksums/manifest/SBOM.

## Связанные материалы

- `README.md`
- `docs/INSTALL.md`
- [Release gate](../VERIFICATION.md#release-gate)
- `.github/workflows/release.yml`
