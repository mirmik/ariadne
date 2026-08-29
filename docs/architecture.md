# Удалённый доступ агента к смартфонам и другим машинам

Архитектурный набросок, 19 августа 2026 года.

## Исходная задача

Нужно дать серверному агенту возможность выполнять задачи на Android-смартфоне, а в перспективе — на любых пользовательских машинах. Агент может запускаться на произвольном сервере, а целевая машина обычно находится за NAT.

Обычный reverse SSH работает, но плохо подходит как пользовательская абстракция:

- приходится помнить динамический порт;
- агент должен находиться на машине, где доступен порт, либо строить ещё один туннель;
- наружу торчат SSH-пользователь, порт и выбор ключа;
- для нескольких устройств возникает ручной реестр портов;
- туннель описывает маршрут, но не identity устройства и не политику доступа.

Главный вывод: **порт не должен быть идентификатором устройства**. Нужен rendezvous-брокер, который маршрутизирует соединения по стабильной identity узла.

## Почему Android-часть лучше строить на Termux

Обычное Android-приложение запускает shell только с UID и разрешениями самого приложения. Оно не получает системный `adb shell`, доступ к данным других приложений или root.

Termux уже предоставляет shell, PTY, пакетный менеджер, SSH, Python/Go, сервисы, автозапуск и Android-возможности через Termux:API. Поэтому на смартфоне логично запускать отдельный `machine-connector` внутри Termux. Migi при этом остаётся изолированным приложением доставки и не получает права исполнять произвольные команды.

Ограничения сохраняются: connector работает с правами Termux, не читает приватные данные других приложений и может быть выгружен Android. Для устойчивости могут понадобиться исключение из оптимизации батареи и аккуратное использование wake lock.

## Предлагаемая архитектура

```text
 Termux / Linux / macOS / Windows connector
                  │
                  │ исходящее WSS/QUIC, mTLS
                  ▼
        ┌──────────────────────────┐
        │ Relay + node registry    │
        │ AuthN/AuthZ + internal CA│
        │ Stream multiplexer       │
        └──────────────────────────┘
                  ▲
                  │ HTTPS / MCP / CLI
                  │
       агент или человек на любой машине
```

У каждого узла есть:

- неизменяемый `node_id`, связанный с публичным ключом;
- удобный alias: `phone`, `home-server`, `buildbox`;
- владелец/namespace;
- labels и capabilities;
- online/offline, версия connector и платформа;
- живое исходящее соединение с relay.

Alias удобен человеку, но криптографической identity является ключ и соответствующий `node_id`.

## Enrollment узла

1. Connector локально генерирует Ed25519-ключ.
2. Показывает ссылку и код GitHub Device Flow.
3. Пользователь подтверждает привязку в браузере.
4. Брокер связывает публичный ключ с identity владельца.
5. Брокер выдаёт сертификат узла, `node_id` и alias.
6. Connector подключается к единому endpoint, например `wss://relay.example/connect`.
7. После обрыва connector переподключается с той же identity; alias и ACL не меняются.

Ручной перенос приватного ключа не нужен. Ключи всё равно существуют, но их жизненный цикл автоматизирован.

## Identity пользователя и агента

Нужно разделять четыре сущности:

1. Identity relay-сервера — TLS-сертификат.
2. Identity целевого узла — ключ/сертификат enrollment.
3. Identity человека — например GitHub OAuth Device Flow.
4. Identity агента/workload — отдельная служебная identity или делегированное право.

Upterm с `--github-user` получает публичные SSH-ключи GitHub-профиля и проверяет владение соответствующим приватным ключом. Это связывает GitHub-профиль с SSH-ключом, но не является OAuth-login и не проверяет identity relay-сервера.

Для постоянной системы GitHub лучше использовать как внешний identity provider, а после входа выдавать короткоживущие внутренние credentials.

Агенту не следует давать постоянный персональный GitHub-токен. Ему нужен ограниченный capability token:

```yaml
subject: agent/codex-task-123
targets:
  - phone
  - buildbox
actions:
  - exec
  - read_files
expires_at: 2026-08-19T20:00:00Z
```

Токен ограничивает targets, actions, срок жизни и при необходимости рабочий каталог, время процесса и объём данных. Интерактивный raw shell должен быть отдельным повышенным правом.

## Data plane

Relay предоставляет мультиплексированные двунаправленные потоки:

```text
open_stream(target=node_id, protocol=ssh|exec|file|mcp)
```

Брокер проверяет права, находит живое соединение узла и связывает потоки. Локального TCP-порта для пользователя не возникает.

Для интерактивного режима разумно сохранить SSH как payload: он уже решает PTY, сигналы, SFTP/SCP, размеры окна и множество каналов. Connector или локальный sshd завершает SSH, relay занимается маршрутизацией.

Для автоматических заданий полезен структурированный exec-протокол:

- shell-строка по умолчанию для удобства агента, с явной семантикой выбранного shell;
- отдельный `argv`-режим для точного запуска без shell;
- `cwd` и ограниченный `env`;
- отдельные stdin/stdout/stderr;
- timeout и cancellation;
- session/job ID;
- лимит вывода и artifacts для больших результатов;
- отдельный интерактивный raw shell.

Собственную криптографию или «почти SSH» разрабатывать не стоит: использовать SSH либо стандартный TLS/mTLS с проверенной протокольной библиотекой.

## Интерфейсы

CLI для человека:

```bash
bridge login
bridge nodes
bridge shell phone
bridge exec buildbox -- uname -a
bridge get phone:/path/file ./file
```

Совместимость с OpenSSH:

```bash
ssh -o ProxyCommand='bridge proxy %h' phone
```

CLI скрывает port, identity file и технического OS-пользователя. Для Termux локальный пользователь один; для Linux соответствие caller identity → OS account задаётся политикой.

MCP лучше держать централизованно рядом с relay:

```text
machines.list
machines.status
machines.exec(target, command, shell, cwd, timeout)
machines.process_start/read/write/kill
machines.files_list/get/put
machines.shell
```

Строковый `command` выбран основным агентским интерфейсом: модели естественно создают консольные команды, пайпы и перенаправления. MCP при этом сохраняет схемы, структурированные результаты, scopes, target identity, аудит и approvals; точный `argv` остаётся доступен там, где shell-интерпретация нежелательна.

MCP является northbound API для модели. Relay/connector образуют southbound data plane и отвечают за мобильное соединение, reconnect и маршрутизацию.

## Онлайн- и офлайн-семантика

Интерактивный shell возможен только при online-узле. Для автоматических задач позднее можно добавить отдельную очередь: подписанное задание с TTL, получение после reconnect и ограниченный artifact с результатом. Не следует изображать offline job как зависшее SSH-соединение.

## Безопасность

- короткоживущие client credentials;
- автоматическая ротация сертификатов connector;
- replay protection, request ID, issued-at и expiry;
- ACL по owner/group/agent/target/action;
- отдельное право на raw shell;
- аудит вызовов;
- локальный видимый выключатель доступа;
- немедленный revoke узла или делегации;
- лимиты времени, памяти, вывода и файлов;
- SSH/TLS-секреты не попадают в prompt или tool arguments;
- желательно end-to-end защищать поток, чтобы relay видел минимум содержимого.

Нужно отдельно решить, записывает ли relay terminal I/O. Это помогает аудиту, но превращает relay в хранилище чувствительных данных.

## Ориентиры

### Upterm

Хороший образец data plane: локальный SSH server, исходящее соединение к `uptermd`, маршрутизация по session token, SSH поверх WebSocket и разрешение GitHub-пользователей через опубликованные SSH-ключи. Но его модель — временная shared terminal session, а не постоянный fleet с alias, RBAC и workload identities.

### Teleport

Хороший образец control plane: узлы регистрируются и держат reverse tunnel к proxy; proxy маршрутизирует по identity; используются внутренние CA, короткоживущие сертификаты, RBAC, аудит и GitHub authentication. Полный Teleport может быть тяжёлым для Termux, но его модель подтверждает направление.

> Искомая система: data plane в духе Upterm + облегчённый control plane в духе Teleport + MCP-фасад для агентов.

## Реалистичный MVP

### Этап 1 — data plane

- один relay endpoint на HTTPS/WSS;
- connector для Linux и Termux;
- автоматически созданный node key;
- постоянный `node_id` и вручную назначенный alias;
- поток до Termux/OpenSSH;
- `bridge shell` и `bridge exec`;
- простой enrollment token.

### Этап 2 — identity

- GitHub Device Flow;
- внутренняя CA;
- короткоживущие credentials;
- namespace и ACL;
- reconnect;
- стабильный target независимо от relay instance;
- registry в БД и распределённый presence.

### Этап 3 — agent interface

- центральный MCP server;
- exec/process/file tools;
- task-scoped capability tokens;
- approvals для raw shell и чувствительных Android API;
- artifacts и лимиты результатов.

### Этап 4 — эксплуатация

- несколько relay instances;
- offline jobs;
- аудит и revoke;
- labels/groups;
- обновление connector;
- Android-specific tools через Termux:API.

## Открытые решения

- Relay только маршрутизирует end-to-end encrypted streams или завершает SSH ради аудита?
- Нужен ли полноценный shell агенту либо достаточно exec/process API?
- Кто выдаёт workload identity агенту на сторонней платформе?
- Нужны ли разовые подтверждения на смартфоне?
- Как хранить offline jobs без раскрытия relay?
- Connector исполняет команды сам или проксирует OpenSSH?
- Нужна ли полная совместимость с `ssh/scp`?

## Ссылки

- Android Application Sandbox: <https://source.android.com/docs/security/app-sandbox>
- Termux: <https://github.com/termux/termux-app>
- Termux:API: <https://github.com/termux/termux-api>
- Upterm: <https://github.com/owenthereal/upterm>
- GitHub Device Flow: <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow>
- Teleport agents: <https://goteleport.com/docs/reference/architecture/agents/>
- MCP transports: <https://modelcontextprotocol.io/specification/2026-07-28/basic/transports>
- MCP tools: <https://modelcontextprotocol.io/specification/2026-07-28/server/tools>
