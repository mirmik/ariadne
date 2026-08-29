# Ariadne wire protocol v1

Это описание текущего экспериментального протокола. Совместимость между версиями пока не обещается.

## Transport и endpoints

Node plane по умолчанию использует отдельный порт `47471`. Доступны два
совместимых транспорта:

- QUIC/TLS 1.3 с ALPN `ariadne/1` на UDP;
- `GET /v1/connect` — постоянный WebSocket connector → relay на TCP;
- `GET /healthz` — health endpoint TCP listener.

Management plane по умолчанию слушает `127.0.0.1:8088`, требует отдельный bearer token и содержит:

- `GET /v1/nodes` — список online-узлов;
- `POST /v1/nodes/{node_id}/claim` — назначение доверенного alias с management plane;
- `POST /v1/nodes/{target}/exec` — структурированный exec;
- `GET /v1/nodes/{target}/streams/shell` — WebSocket до встроенного одноразового SSH endpoint;
- `GET /v1/nodes/{target}/streams/ssh` — опциональный WebSocket byte-stream до внешнего локального `sshd`;
- `GET /healthz` — health endpoint.

Node plane не требует предварительно доставленного bearer token: connector доказывает владение своей Ed25519 identity во время handshake. Он не предоставляет endpoints для управления. Management plane использует отдельный случайный 256-bit bearer token; relay создаёт token-файл `0600`, а `ari` читает его локально. Этот административный credential не входит в node bootstrap. Публичный node plane использует QUIC/TLS или HTTPS/WSS.

Для прямого доступа роутер может пробросить UDP и TCP одного внешнего порта на `47471`: connector принимает короткий `--relay HOST` как `quic://HOST:47471`, предпочитает QUIC и затем использует WSS fallback. `HOST:PORT` меняет порт, а полный URL явно выбирает transport. TLS identity relay проверяется по TOFU: первый сертификат для `host:port` сохраняется в пользовательском `known_relays`, последующие подключения требуют точного совпадения. Изменившийся сертификат блокируется; одноразовая замена требует `--accept-new-relay-certificate`. Для заранее подготовленной установки TOFU можно заменить точным публичным pin из `--relay-cert-pin`. Старый bootstrap через `ariadne-connector --relay-ssh breakglass@HOST[:PORT]` создаёт SSH local forward до relay `127.0.0.1:47471` и остаётся доступен как fallback для сетей без UDP. Management client `ari` обычно работает рядом с relay или через защищённый tunnel и должен иметь token-файл. Plaintext non-loopback listener требует явный insecure-флаг.

Переданный connector alias является недоверенной подсказкой. В списке узлов он имеет `alias_claimed: false` и не участвует в lookup. Management plane может связать alias с точным `node_id` через `/claim`; только такой alias разрешено использовать как target. Claims сохраняются после reconnect той же identity, но в v1 теряются при перезапуске relay.

## Регистрация connector

Control-сообщения являются текстовыми messages с envelope:

```json
{
  "version": 1,
  "type": "connector.hello",
  "id": "optional-request-id",
  "payload": {}
}
```

В WSS message boundaries задаёт WebSocket. В QUIC v1 connector открывает один
bidirectional control stream; поверх него message кодируется как `type: u8`,
`length: u32 big-endian`, `payload`. Типы `1` и `2` соответствуют text и binary,
а `3`/`4` — ping/pong. Лимит одного message — 4 MiB. Вложенные shell/SSH
потоки пока используют тот же application-level stream ID framing, поэтому
переезд транспорта не меняет semantics wire v1. Нативные QUIC streams можно
добавить следующей версией протокола без изменения регистрации.

Handshake:

1. Connector отправляет `connector.hello`: `node_id`, alias, Ed25519 public key, встроенный SSH host key, platform, architecture и версию.
2. Relay проверяет, что `node_id` получен из public key, и отвечает случайным `relay.challenge`.
3. Connector подписывает domain-separated transcript, включающий challenge и все поля hello, и отправляет `connector.register`.
4. Relay проверяет Ed25519 signature, регистрирует live identity и отвечает `relay.registered`.

Приватный ключ никогда не покидает узел. Challenge не даёт повторно воспроизвести перехваченную регистрацию. `ssh_host_key` входит в подписываемый transcript: relay не принимает его замену без новой корректной подписи node identity.

SSH host key стабильно выводится из seed node identity через отдельный HMAC-SHA-256 domain label `ariadne/ssh-host/v1`. Это даёт стабильность после reconnect и одновременно не переиспользует один Ed25519 private key в двух протоколах.

## Exec

Management HTTP/MCP request принимает высокоуровневую форму:

```json
{
	"command": "uname -a | sed -n '1p'",
	"shell": "auto",
	"cwd": "/tmp",
	"timeout_ms": 30000
}
```

`command` является основным агентским форматом. `auto` выбирает PowerShell на Windows и POSIX shell на остальных поддерживаемых платформах; явно доступны `posix`, `powershell` и `cmd`. Relay преобразует строку в точный shell `argv` перед отправкой connector, поэтому этот режим совместим с ранее выпущенными connector. На Android системный `/system/bin/sh` служит только trampoline до Termux `sh`: сама команда передаётся отдельным аргументом, без повторного строкового экранирования. Ответ `exec.result` содержит фактически выбранный shell.

Для запуска без shell вместо `command` передаётся `argv`; эти поля взаимоисключающие:

```json
{"argv": ["uname", "-a"], "timeout_ms": 30000}
```

Connector всегда запускает полученный `argv` напрямую и отвечает exit code, раздельными stdout/stderr, duration и признаками timeout/truncation. `exec.cancel` отменяет процесс, если HTTP client исчез или истёк relay timeout.

stdout и stderr являются byte strings и кодируются стандартным JSON base64, поэтому бинарный вывод не повреждается.

Connector передаёт процессу своё окружение целиком, чтобы системные и toolchain-команды работали так же, как при локальном запуске. Environment не является security boundary: команда работает с тем же OS UID и в зависимости от платформы может исследовать другие процессы и доступные им файлы. Connector не следует запускать с секретами в environment; настоящая изоляция требует отдельного UID или sandbox. Вывод ограничен отдельно для stdout и stderr.

## Встроенный SSH shell

`ari shell TARGET` не использует пользовательские ключи или `authorized_keys`:

1. CLI создаёт новую Ed25519 keypair только в памяти процесса.
2. Публичная часть передаётся relay в заголовке `X-Ariadne-SSH-Client-Key` запроса `/streams/shell`.
3. Relay выбирает текущую зарегистрированную node-сессию, возвращает её `X-Ariadne-Node-ID` и подписанный при регистрации `X-Ariadne-SSH-Host-Key`, затем вкладывает client key в `stream.open`.
4. Connector создаёт SSH server непосредственно поверх этого stream без TCP listener. Его `PublicKeyCallback` принимает только ключ конкретного `stream.open`, только пользователя протокола `ariadne` и не включает password auth.
5. CLI использует `ssh.FixedHostKey` с ключом из handshake relay. Режим, эквивалентный `InsecureIgnoreHostKey`, не используется.
6. После SSH handshake connector обслуживает один `session` channel: `pty-req`, `shell`, `window-change`, ограниченный набор `signal` и `exit-status`.

При PTY shell запускается на pseudo-terminal с переданными размерами; без PTY stdin/stdout/stderr подключаются напрямую. Процесс работает с UID connector и наследует то же окружение, что и structured exec. SSH-шифрование находится внутри внешнего HTTPS/WSS transport; relay видит routing metadata и публичные ключи, но не расшифровывает SSH payload.

## Мультиплексированные stream frames

Когда клиент подключается к `/streams/shell` или `/streams/ssh`, relay создаёт случайный 128-bit stream ID и отправляет connector сообщение `stream.open`. Поле `protocol` имеет значение `shell` для встроенного endpoint или `ssh` для внешнего proxy. Для `shell` сообщение также содержит одноразовый `ssh_client_public_key`. Для `ssh` connector сам выбирает заранее настроенный адрес локального `sshd`; relay не может попросить его соединиться с произвольным host/port.

После `stream.opened` данные передаются бинарными messages по постоянному connector-соединению:

```text
byte 0       protocol version (1)
byte 1       flags (0 in v1)
bytes 2..17  raw 128-bit stream ID
bytes 18..   opaque stream payload, at most 64 KiB
```

Сообщения `stream.close` и `stream.error` управляют жизненным циклом. Несколько SSH-сессий используют одно connector-соединение и различаются stream ID.

Connector является строго реактивной стороной после регистрации. `exec.result` принимается только для существующего relay request ID, а stream state и бинарные frames — только для stream ID, ранее созданного management plane. Unsolicited result или никогда не выдававшийся relay stream ID считаются нарушением протокола и закрывают node connection; запоздалые frames уже закрытого, но ранее выданного stream безопасно отбрасываются.

Клиентский WebSocket содержит только payload bytes без внутреннего заголовка. `ari shell` передаёт его встроенному Go SSH client, а `ari proxy` преобразует в обычный stdin/stdout byte-stream для OpenSSH `ProxyCommand`.

## Неизменяемые ограничения v1

- control message не превышает 4 MiB;
- stream payload не превышает 64 KiB;
- alias соответствует `[A-Za-z0-9][A-Za-z0-9._-]{0,62}` и сравнивается без учёта регистра;
- node identity — Ed25519 public key, `node_id` — 160 бит SHA-256 digest в base32;
- встроенные SSH host key и одноразовые client keys — Ed25519;
- сообщённые aliases могут повторяться и не участвуют в lookup; claimed alias уникален без учёта регистра;
- reconnect той же identity заменяет старую live-сессию.
