# Ariadne wire protocol v1

Это описание текущего экспериментального протокола. Совместимость между версиями пока не обещается.

## Transport и endpoints

- `GET /v1/connect` — постоянный WebSocket connector → relay;
- `GET /v1/nodes` — список online-узлов;
- `POST /v1/nodes/{target}/exec` — структурированный exec;
- `GET /v1/nodes/{target}/streams/ssh` — WebSocket byte-stream клиента до SSH endpoint узла;
- `GET /healthz` — незакрытый health endpoint.

Все endpoints, кроме `/healthz`, требуют `Authorization: Bearer …`, если relay не запущен в явно небезопасном режиме. В production используется HTTPS/WSS.

## Регистрация connector

Control-сообщения являются текстовыми WebSocket messages с envelope:

```json
{
  "version": 1,
  "type": "connector.hello",
  "id": "optional-request-id",
  "payload": {}
}
```

Handshake:

1. Connector отправляет `connector.hello`: `node_id`, alias, Ed25519 public key, platform, architecture и версию.
2. Relay проверяет, что `node_id` получен из public key, и отвечает случайным `relay.challenge`.
3. Connector подписывает domain-separated transcript, включающий challenge и все поля hello, и отправляет `connector.register`.
4. Relay проверяет Ed25519 signature, резервирует alias и отвечает `relay.registered`.

Приватный ключ никогда не покидает узел. Challenge не даёт повторно воспроизвести перехваченную регистрацию.

## Exec

HTTP request преобразуется relay в `exec.request`:

```json
{
  "argv": ["uname", "-a"],
  "cwd": "/tmp",
  "timeout_ms": 30000
}
```

Connector запускает `argv` напрямую и отвечает `exec.result` с exit code, раздельными stdout/stderr, duration и признаками timeout/truncation. `exec.cancel` отменяет процесс, если HTTP client исчез или истёк relay timeout.

stdout и stderr являются byte strings и кодируются стандартным JSON base64, поэтому бинарный вывод не повреждается.

По умолчанию connector передаёт процессу только небольшой allowlist переменных окружения; bearer token напрямую не наследуется командой. Это не создаёт security boundary: команда работает с тем же OS UID и в зависимости от платформы может исследовать другие процессы и доступные им файлы. Настоящая изоляция credentials требует отдельного UID или sandbox. Вывод ограничен отдельно для stdout и stderr.

## SSH streams

Когда клиент подключается к `/streams/ssh`, relay создаёт случайный 128-bit stream ID и отправляет connector сообщение `stream.open`. Connector сам выбирает заранее настроенный адрес локального `sshd`; relay не может попросить его соединиться с произвольным host/port.

После `stream.opened` данные передаются бинарными WebSocket messages по постоянному connector-соединению:

```text
byte 0       protocol version (1)
byte 1       flags (0 in v1)
bytes 2..17  raw 128-bit stream ID
bytes 18..   opaque stream payload, at most 64 KiB
```

Сообщения `stream.close` и `stream.error` управляют жизненным циклом. Несколько SSH-сессий используют одно connector-соединение и различаются stream ID.

Клиентский WebSocket содержит только payload bytes без внутреннего заголовка. `ari proxy` преобразует его в обычный stdin/stdout byte-stream для OpenSSH `ProxyCommand`.

## Неизменяемые ограничения v1

- control message не превышает 4 MiB;
- stream payload не превышает 64 KiB;
- alias соответствует `[A-Za-z0-9][A-Za-z0-9._-]{0,62}` и сравнивается без учёта регистра;
- node identity — Ed25519 public key, `node_id` — 160 бит SHA-256 digest в base32;
- duplicate alias разных identities отклоняется;
- reconnect той же identity заменяет старую live-сессию.
