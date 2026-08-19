# Ariadne

Ariadne даёт агенту или человеку доступ к машине за NAT по стабильному имени. Машина сама держит исходящее WebSocket-соединение с relay; входящий SSH-порт ей не нужен.

Репозиторий пока содержит экспериментальный, но уже сквозной MVP:

- `ariadne-relay` — registry online-узлов и маршрутизация потоков;
- `ariadne-connector` — исходящее соединение, постоянная Ed25519 identity, forwarding до локального `sshd` и структурированный exec;
- `ari` — команды `nodes`, `exec` и OpenSSH `ProxyCommand`;
- challenge-response регистрации, bearer-аутентификация, reconnect, лимиты времени, параллелизма и вывода;
- unit-тесты и интеграционный тест полного relay → connector пути.

## Как устроен текущий data plane

```text
OpenSSH / ari exec
       │ HTTPS/WSS
       ▼
  ariadne-relay
       ▲
       │ исходящее WSS
       │
ariadne-connector ── TCP ── localhost:sshd
```

SSH остаётся end-to-end протоколом между OpenSSH и удалённым `sshd`. Relay переносит непрозрачные SSH-байты в мультиплексированных stream-кадрах. Структурированный `exec` идёт отдельными control-сообщениями и запускает `argv` напрямую, без shell-строки.

## Сборка

Нужен Go 1.24 или новее.

```bash
mkdir -p bin
go build -o bin/ari ./cmd/ari
go build -o bin/ariadne-relay ./cmd/ariadne-relay
go build -o bin/ariadne-connector ./cmd/ariadne-connector
```

## Локальная проверка

Создайте один и тот же случайный токен в окружении relay, connector и CLI:

```bash
export ARIADNE_TOKEN="$(openssl rand -hex 32)"
./bin/ariadne-relay
```

В другом терминале:

```bash
export ARIADNE_TOKEN="тот-же-токен"
./bin/ariadne-connector --alias phone --ssh-address 127.0.0.1:8022
```

Проверка registry и структурированного exec:

```bash
./bin/ari nodes
./bin/ari exec phone -- uname -a
./bin/ari exec --timeout 5s --cwd /tmp phone -- pwd
```

Identity connector создаётся один раз в пользовательском config-каталоге с правами `0600`. `node_id` является хешем публичного ключа и не меняется после reconnect.

## SSH через relay

На Termux установите и запустите OpenSSH:

```bash
pkg install openssh
sshd
```

Termux обычно слушает `127.0.0.1:8022`, поэтому это значение является default для connector. На обычном Linux передайте `--ssh-address 127.0.0.1:22`.

Если статически собранный Go-бинарник в Termux не находит системные TLS roots, явно укажите пакетный CA bundle перед подключением к WSS:

```bash
export SSL_CERT_FILE="$PREFIX/etc/tls/cert.pem"
```

Прямая команда OpenSSH:

```bash
ssh -o "ProxyCommand=/absolute/path/to/ari --relay https://relay.example proxy %h" termux-user@phone
```

Или запись в `~/.ssh/config`:

```sshconfig
Host phone
    User termux-user
    ProxyCommand /absolute/path/to/ari --relay https://relay.example proxy %h
```

Обычные SSH host keys, user keys и `authorized_keys` продолжают работать. `ARIADNE_TOKEN` должен присутствовать в окружении процесса `ssh`, откуда его наследует ProxyCommand.

## Публичный relay

Без TLS relay по умолчанию разрешено слушать только на loopback. Для прямой публикации настройте сертификат:

```bash
./bin/ariadne-relay \
  --listen 0.0.0.0:443 \
  --tls-cert /path/fullchain.pem \
  --tls-key /path/privkey.pem
```

Также можно оставить relay на loopback за HTTPS reverse proxy. Connector и CLI принимают `https://relay.example` и автоматически используют WSS для потоков.

Флаги `--insecure-no-auth`, `--allow-insecure-listen` и `--allow-insecure-relay` существуют только для явных локальных экспериментов.

## Текущие ограничения

- один общий bearer token вместо enrollment-токенов и action-scoped capabilities;
- registry хранится в памяти, а offline nodes/jobs пока отсутствуют;
- одна relay-инстанция без распределённого presence;
- внешний `sshd` должен быть установлен и запущен отдельно;
- нет namespace, ACL, approvals, внутренней CA и mTLS;
- `exec` не является sandbox: команда получает права OS-пользователя connector;
- connector и запущенная команда пока разделяют один OS UID, поэтому shared token нельзя считать изолированным от недоверенной команды;
- terminal I/O не записывается, SSH-содержимое relay не расшифровывает.

Исходный архитектурный набросок находится в [docs/architecture.md](docs/architecture.md), текущий wire-протокол — в [docs/protocol.md](docs/protocol.md).
