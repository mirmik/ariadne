# Ariadne

Ariadne даёт агенту или человеку доступ к машине за NAT по стабильному имени. Машина сама держит исходящее QUIC- или WebSocket-соединение с relay; входящий порт, отдельный `sshd` и ручная раскладка SSH-ключей для минимального сценария не нужны.

Репозиторий пока содержит экспериментальный, но уже сквозной MVP:

- `ariadne-relay` — registry online-узлов и маршрутизация потоков;
- `ariadne-connector` — исходящее соединение, постоянная Ed25519 identity, встроенный SSH endpoint, PTY, опциональный forwarding до локального `sshd` и структурированный exec;
- `ari` — команды `nodes`, `exec`, zero-config `shell` и OpenSSH-совместимый `proxy`;
- challenge-response регистрации без bootstrap-секрета, reconnect, лимиты времени, параллелизма и вывода;
- unit-тесты и интеграционный тест полного relay → connector пути.

## Как устроен текущий data plane

```text
ari shell / ari exec
        │ authenticated management HTTP через loopback/SSH tunnel (:8088)
        ▼
   ariadne-relay
        ▲
        │ исходящее QUIC/UDP, с WSS/TCP fallback (:47471)
        │
 ariadne-connector ── PTY ── shell того же OS-пользователя
```

`ari shell` поднимает SSH непосредственно внутри одного Ariadne stream: локальный TCP listener не создаётся. CLI генерирует одноразовый Ed25519-ключ в памяти, connector принимает его только для этого потока, а CLI проверяет host key, привязанный к подписанной node identity. Relay переносит непрозрачные SSH-байты в мультиплексированных stream-кадрах. Структурированный `exec` идёт отдельными control-сообщениями и запускает `argv` напрямую, без shell-строки.

`ari proxy` сохранён как совместимый низкоуровневый путь для обычного OpenSSH. Только этот режим требует локальный `sshd` и его штатные user keys/`authorized_keys`.

## Сборка

Нужен Go 1.26.6 или новее.

Готовые connector-бинарники для Windows, Linux и Android/Termux публикуются в
[GitHub Releases](https://github.com/mirmik/ariadne/releases). Для обычного
64-битного Windows нужен файл `ariadne-connector-windows-amd64.exe`. В Windows
режим `--relay-ssh` использует системный клиент OpenSSH (`ssh.exe`):

```powershell
.\ariadne-connector-windows-amd64.exe --relay-ssh breakglass@relay-host --alias workstation
```

Connector создаст постоянную identity в пользовательском config-каталоге и
попросит временный break-glass пароль через OpenSSH. Контрольные суммы файлов
публикуются рядом в `SHA256SUMS`.

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
task claim NODE_ID=идентификатор-из-nodes
task shell
task exec -- uname -a
```

Значения из `.env.local` можно переопределять для одной команды:

```bash
task connector NODE_RELAY_SSH=breakglass@relay-host NODE_ALIAS=tablet
task shell MANAGEMENT_RELAY=http://127.0.0.1:18088 NODE=tablet
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

```bash
./bin/ariadne-relay
```

В другом терминале:

```bash
./bin/ariadne-connector --alias phone
```

`--ssh-address` влияет только на опциональный `ari proxy`; для `ari shell` этот адрес не используется, поэтому флаг можно не указывать.

Проверка registry и структурированного exec:

```bash
./bin/ari nodes
./bin/ari claim NODE_ID phone
./bin/ari exec phone -- uname -a
./bin/ari exec --timeout 5s --cwd /tmp phone -- pwd
./bin/ari shell phone
```

## MCP и skill для агентов

`ariadne-mcp` предоставляет management plane как локальный stdio MCP без shell-вызовов `ari`:

- `ariadne_nodes` — живые ноды, platform и статус доверенного alias;
- `ariadne_claim` — привязка alias к точному `node_id`;
- `ariadne_exec` — прямой `argv`, remote `cwd`, timeout и структурированные stdout/stderr/exit code.

MCP запускается рядом с relay, читает тот же локальный management token и не передаёт его connector. Интерактивный shell не включён в MCP: stdio занят протоколом, а агентские операции должны использовать structured exec.

Для Codex соберите MCP, установите user-wide skill и зарегистрируйте сервер одной командой:

```bash
./scripts/install-ariadne-agent-tools
```

Skill устанавливается как ссылка в `~/.agents/skills/ariadne-remote`, а MCP-бинарник — в `~/.local/bin/ariadne-mcp`. Codex CLI, IDE extension и desktop app используют общую MCP-конфигурацию. Для remote-команд дольше стандартных 60 секунд установите `mcp_servers.ariadne.tool_timeout_sec = 660` в `~/.codex/config.toml`.

Ручной запуск для другого MCP-клиента:

```bash
./bin/ariadne-mcp
```

Identity connector создаётся один раз в пользовательском config-каталоге с правами `0600`. `node_id` является хешем публичного ключа и не меняется после reconnect. Стабильный SSH host key детерминированно и с отдельным domain label выводится из этой identity, но не переиспользует сам identity key.

При первом запуске relay создаёт отдельный 256-bit management token в
пользовательском config-каталоге (`$XDG_CONFIG_HOME/ariadne/management.token`,
обычно `~/.config/ariadne/management.token`) с правами `0600`. `ari` по
умолчанию читает тот же файл и передаёт token как bearer credential для всех
management HTTP и WebSocket запросов. Путь с обеих сторон можно изменить через
`--management-token-file`. Этот token никогда не передаётся connector и не
участвует в bootstrap node identity.

Alias, сообщённый новым connector, отображается в `ari nodes` с суффиксом `?` и не используется как target. Управляющая сторона должна один раз выполнить `ari claim NODE_ID ALIAS`; claim сохраняется в памяти relay, применяется после reconnect и делает alias доступным для `shell`/`exec`. Постоянное хранение claims пока не реализовано.

## Zero-config shell

Для интерактивного доступа достаточно:

```bash
./bin/ari shell phone
```

Если stdin является терминалом, CLI включает raw mode, запрашивает PTY и передаёт изменения размера окна. Если stdin — pipe, shell запускается без PTY, поэтому команду можно использовать и в простом скрипте:

```bash
printf 'uname -a\nexit\n' | ./bin/ari shell phone
```

Удалённый shell выбирается из `SHELL`, затем из `PATH` как `sh`; при необходимости connector принимает `--shell /absolute/path`. Дочерний процесс наследует окружение connector целиком, чтобы системные и toolchain-команды работали так же, как при локальном запуске. Не передавайте connector секреты через environment; для настоящей изоляции запускайте его под отдельным OS-пользователем или в sandbox.

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

Обычные SSH host keys, user keys и `authorized_keys` продолжают работать.

## Доступ connector к node plane

Relay запускает два независимых HTTP server. Management plane слушает `127.0.0.1:8088`, connector-facing node plane — `127.0.0.1:47471`. Если через роутер уже доступен SSH, node port пробрасывать не требуется: внешний connector сам поднимает ограниченный tunnel через `breakglass`:

```bash
ariadne-connector \
  --relay-ssh breakglass@relay-host:22061 \
  --alias phone
```

SSH port после двоеточия необязателен; без него OpenSSH использует config или порт `22`. OpenSSH запрашивает временный break-glass пароль, создаёт local forward до `127.0.0.1:47471` на relay и остаётся дочерним процессом connector. После истечения password TTL установленный tunnel продолжает работать. Если SSH-соединение оборвётся, connector завершится: для нового входа нужно снова открыть break-glass окно и перезапустить connector.

`ari` в основном сценарии работает рядом с relay и обращается к `http://127.0.0.1:8088`, используя локальный management token. Для другой доверенной management-машины token-файл нужно безопасно доставить отдельно, а соединение провести через SSH tunnel или TLS. Явный `--allow-insecure-management-listen` разрешает plaintext bearer token на non-loopback адресе и предназначен только для изолированной доверенной сети. Режим `ari --relay-ssh` сохранён как вспомогательный вариант; на машине с `ari` всё равно должен быть management token-файл.

Relay может публиковать management plane напрямую с TLS. Token-файл нужно
отдельно и безопасно доставить только доверенным управляющим машинам:

```bash
ariadne-relay \
  --management-listen 192.168.0.61:8088 \
  --management-tls-cert /path/management.crt \
  --management-tls-key /path/management.key

ariadne-mcp \
  --relay https://192.168.0.61:8088 \
  --management-token-file ~/.config/ariadne/management.token
```

Для сертификата от локальной CA укажите доверенный CA bundle через штатный
`SSL_CERT_FILE` окружения процесса `ariadne-mcp`. Management TLS и node TLS
настраиваются независимо.

Для прямого подключения удалённых нод relay может слушать QUIC/UDP и WSS/TCP
на одном номере порта. Connector доверяет TLS-сертификату по модели TOFU,
аналогичной SSH `StrictHostKeyChecking=accept-new`: при первом подключении
запоминает отпечаток для `host:port`, а затем требует точного совпадения. Для
короткой записи bare host означает `quic://HOST:47471`, а `HOST:PORT` — QUIC с
явным портом. Полный URL переопределяет эти defaults:

```bash
./bin/ariadne-relay \
  --management-listen 127.0.0.1:8088 \
  --node-listen 192.168.0.61:47471 \
  --node-loopback-listen 127.0.0.1:47471 \
  --node-quic-listen 192.168.0.61:47471 \
  --node-tls-cert /path/fullchain.pem \
  --node-tls-key /path/privkey.pem

./bin/ariadne-connector \
  --relay relay.example \
  --alias workstation
```

Trust store находится в `~/.config/ariadne/known_relays` и создаётся с правами
`0600`. QUIC и WSS на одном `host:port` используют одну запись. Если relay
предъявил другой сертификат, connector блокирует соединение и печатает старый и
новый отпечатки. После независимой проверки новой identity пользователь может
явно принять ровно одну замену:

```bash
./bin/ariadne-connector \
  --relay relay.example \
  --accept-new-relay-certificate \
  --alias workstation
```

Как и у SSH TOFU, самое первое подключение уязвимо для активного MITM. Для
unattended provisioning с заранее известной identity сохранён
`--relay-cert-pin sha256:HEX_DIGEST`; при его наличии trust store не
используется. Путь store можно изменить через `--known-relays-file`.

Для автоматического fallback нужно пробросить на relay и UDP, и TCP `47471`;
connector сначала использует QUIC, а через четыре секунды пробует
`https://relay.example:47471/v1/connect`. Fallback можно отключить флагом
`--relay-fallback none` или заменить отдельным URL. Management plane через
роутер не публикуется.
Дополнительный `--node-loopback-listen` сохраняет plaintext endpoint только на
loopback для режима `--relay-ssh`; наружу он не доступен.

При необходимости разового внешнего управления `ari` может создать вспомогательный tunnel:

```bash
ari --relay-ssh breakglass@relay-host nodes
ari --relay-ssh breakglass@relay-host shell phone
```

Обе программы используют системный OpenSSH и выбирают свободный loopback-порт. Ручной `ssh -L` также поддерживается.

У connector нет bootstrap bearer token. Публичный node plane принимает новые self-authenticated Ed25519 identities, но не содержит управляющих endpoints. QUIC использует TLS 1.3, ALPN `ariadne/1`, отключённый 0-RTT и keepalive; при разрыве connector переподключается с той же identity. Отдельный management bearer token защищает `nodes`, `claim`, `exec` и stream endpoints даже на loopback. Management plane по умолчанию разрешено привязать только к loopback. Флаги `--allow-insecure-management-listen`, `--allow-insecure-node-listen` и `--allow-insecure-relay` предназначены для явных plaintext-экспериментов.

## Текущие ограничения

- неизвестные node identities допускаются без enrollment; registry и доверенные aliases пока не сохраняются;
- registry хранится в памяти, а offline nodes/jobs пока отсутствуют;
- одна relay-инстанция без распределённого presence;
- нет namespace, ACL, approvals, внутренней CA и mTLS;
- `exec` не является sandbox: команда получает права OS-пользователя connector;
- connector, `exec` и встроенный shell пока разделяют один OS UID;
- неподтверждённые aliases являются только метаданными и не используются для lookup; claim пока хранится только в памяти;
- relay проверяет подписанную регистрацию и сообщает CLI привязанный SSH host key, но постоянный registry и claim-модель aliases ещё не реализованы;
- terminal I/O не записывается, SSH-содержимое relay не расшифровывает.

Исходный архитектурный набросок находится в [docs/architecture.md](docs/architecture.md), текущий wire-протокол — в [docs/protocol.md](docs/protocol.md).

## Дополнительные инструменты

В репозитории также находится независимая серверная утилита `ssh-breakglass`: она позволяет с доверенного телефона на короткое время включить случайный пароль для отдельного SSH-пользователя и автоматически блокирует его systemd-таймером. Установка и модель безопасности описаны в [docs/ssh-breakglass.md](docs/ssh-breakglass.md).
