# Ariadne

Ariadne даёт агенту или человеку доступ к машине за NAT по стабильному имени. Машина сама держит исходящее WebSocket-соединение с relay; входящий порт, отдельный `sshd` и ручная раскладка SSH-ключей для минимального сценария не нужны.

Репозиторий пока содержит экспериментальный, но уже сквозной MVP:

- `ariadne-relay` — registry online-узлов и маршрутизация потоков;
- `ariadne-connector` — исходящее соединение, постоянная Ed25519 identity, встроенный SSH endpoint, PTY, опциональный forwarding до локального `sshd` и структурированный exec;
- `ari` — команды `nodes`, `exec`, zero-config `shell` и OpenSSH-совместимый `proxy`;
- challenge-response регистрации, bearer-аутентификация, reconnect, лимиты времени, параллелизма и вывода;
- unit-тесты и интеграционный тест полного relay → connector пути.

## Как устроен текущий data plane

```text
ari shell / ari exec
        │ HTTPS/WSS
        ▼
   ariadne-relay
        ▲
        │ исходящее WSS
        │
 ariadne-connector ── PTY ── shell того же OS-пользователя
```

`ari shell` поднимает SSH непосредственно внутри одного Ariadne stream: локальный TCP listener не создаётся. CLI генерирует одноразовый Ed25519-ключ в памяти, connector принимает его только для этого потока, а CLI проверяет host key, привязанный к подписанной node identity. Relay переносит непрозрачные SSH-байты в мультиплексированных stream-кадрах. Структурированный `exec` идёт отдельными control-сообщениями и запускает `argv` напрямую, без shell-строки.

`ari proxy` сохранён как совместимый низкоуровневый путь для обычного OpenSSH. Только этот режим требует локальный `sshd` и его штатные user keys/`authorized_keys`.

## Сборка

Нужен Go 1.24 или новее.

```bash
mkdir -p bin
go build -o bin/ari ./cmd/ari
go build -o bin/ariadne-relay ./cmd/ariadne-relay
go build -o bin/ariadne-connector ./cmd/ariadne-connector
```

### Через Taskfile

Если установлен [Task](https://taskfile.dev/), тот же рабочий цикл короче:

```bash
cp .env.example .env.local
# Заполните ARIADNE_TOKEN в .env.local, например результатом openssl rand -hex 32

task --list
task build
task check
```

Для локального сквозного запуска откройте три терминала в репозитории:

```bash
# Терминал 1
task relay

# Терминал 2
task connector

# Терминал 3
task nodes
task shell
task exec -- uname -a
```

Значения из `.env.local` можно переопределять для одной команды:

```bash
task connector RELAY=https://relay.example NODE_ALIAS=tablet
task shell RELAY=https://relay.example NODE=tablet
task exec NODE=tablet EXEC_FLAGS='--timeout 5s --cwd /tmp' -- pwd
```

Сборка connector и CLI для Termux на обычном Android/ARM64:

```bash
task build:android
# dist/ariadne-connector-android-arm64
# dist/ari-android-arm64
```

Для Linux/ARM64 аналогично доступен `task build:linux-arm64`. Переменные `BIN_DIR`, `DIST_DIR` и `ARCH` переопределяют каталоги локальной сборки, release-каталог и Android-архитектуру соответственно.

## Локальная проверка

Создайте один и тот же случайный токен в окружении relay, connector и CLI:

```bash
export ARIADNE_TOKEN="$(openssl rand -hex 32)"
./bin/ariadne-relay
```

В другом терминале:

```bash
export ARIADNE_TOKEN="тот-же-токен"
./bin/ariadne-connector --alias phone
```

`--ssh-address` влияет только на опциональный `ari proxy`; для `ari shell` этот адрес не используется, поэтому флаг можно не указывать.

Проверка registry и структурированного exec:

```bash
./bin/ari nodes
./bin/ari exec phone -- uname -a
./bin/ari exec --timeout 5s --cwd /tmp phone -- pwd
./bin/ari shell phone
```

Identity connector создаётся один раз в пользовательском config-каталоге с правами `0600`. `node_id` является хешем публичного ключа и не меняется после reconnect. Стабильный SSH host key детерминированно и с отдельным domain label выводится из этой identity, но не переиспользует сам identity key.

## Zero-config shell

Для интерактивного доступа достаточно:

```bash
./bin/ari shell phone
```

Если stdin является терминалом, CLI включает raw mode, запрашивает PTY и передаёт изменения размера окна. Если stdin — pipe, shell запускается без PTY, поэтому команду можно использовать и в простом скрипте:

```bash
printf 'uname -a\nexit\n' | ./bin/ari shell phone
```

Удалённый shell выбирается из `SHELL`, затем из `PATH` как `sh`; при необходимости connector принимает `--shell /absolute/path`. Дочерний процесс получает allowlist окружения без `ARIADNE_TOKEN`, но работает с правами того же OS-пользователя, что и connector.

## Опциональный OpenSSH через relay

Если нужна совместимость с обычным OpenSSH, на Termux можно отдельно установить и запустить сервер:

```bash
pkg install openssh
sshd
```

Termux обычно слушает `127.0.0.1:8022`, поэтому это значение является default для connector. На обычном Linux передайте `--ssh-address 127.0.0.1:22`. Это не влияет на встроенный `ari shell`.

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
- нет namespace, ACL, approvals, внутренней CA и mTLS;
- `exec` не является sandbox: команда получает права OS-пользователя connector;
- connector, `exec` и встроенный shell пока разделяют один OS UID, поэтому shared token нельзя считать изолированным от недоверенной команды;
- relay проверяет подписанную регистрацию и сообщает CLI привязанный SSH host key, но постоянный registry и отдельная enrollment-модель ещё не реализованы;
- terminal I/O не записывается, SSH-содержимое relay не расшифровывает.

Исходный архитектурный набросок находится в [docs/architecture.md](docs/architecture.md), текущий wire-протокол — в [docs/protocol.md](docs/protocol.md).
