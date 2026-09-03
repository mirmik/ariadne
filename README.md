# Ariadne

Ariadne даёт агенту или человеку доступ к машине за NAT по стабильному имени. Машина сама держит исходящее QUIC- или WebSocket-соединение с relay; входящий порт, отдельный `sshd` и ручная раскладка SSH-ключей для минимального сценария не нужны.

Репозиторий пока содержит экспериментальный, но уже сквозной MVP:

- `ariadne-relay` — registry online-узлов и маршрутизация потоков;
- `ariadne-connector` — исходящее соединение, постоянная Ed25519 identity, встроенный SSH endpoint, PTY, опциональный forwarding до локального `sshd`, структурированный exec и атомарная передача файлов;
- `ari` — команды `nodes`, `exec`, zero-config `shell` и OpenSSH-совместимый `proxy`;
- PAKE commissioning для первого знакомства, challenge-response регистрации,
  reconnect и лимиты времени, параллелизма и вывода;
- unit-тесты и интеграционный тест полного relay → connector пути.

Наглядное объяснение первого знакомства и последующего handshake:
[HTML-справка для начинающих](docs/handshake.html).

## Как устроен текущий data plane

```text
ari shell / ari exec
        │ authenticated management HTTP через loopback/SSH tunnel (:8088)
        ▼
   ariadne-relay
        ▲
        │ исходящее QUIC/UDP, с WSS/TCP fallback (:14771 рекомендуется)
        │
 ariadne-connector ── PTY ── shell того же OS-пользователя
```

`ari shell` поднимает SSH непосредственно внутри одного Ariadne stream: локальный TCP listener не создаётся. CLI генерирует одноразовый Ed25519-ключ в памяти, connector принимает его только для этого потока, а CLI проверяет host key, привязанный к подписанной node identity. Relay переносит непрозрачные SSH-байты в мультиплексированных stream-кадрах. Неинтерактивный `exec` принимает удобную командную строку для нативного shell узла; точный `argv` без shell сохранён как низкоуровневая альтернатива.

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
./bin/ariadne-connector
```

Без `--alias` connector использует нормализованный hostname машины. Явный
`--alias phone` по-прежнему имеет приоритет и удобен для заранее выбранного
имени; management plane всё равно должен один раз подтвердить alias через
`ari claim`.

### Автозапуск connector

Connector умеет установить себя в постоянный пользовательский каталог и
зарегистрировать автозапуск без запроса или сохранения пароля:

```bash
ariadne-connector autostart install --relay 95.165.81.109
ariadne-connector autostart status
ariadne-connector autostart uninstall
```

Аргументы после `install` сохраняются как отдельный массив, без повторного
разбора shell-командной строки. При первой установке `--relay` (или
`--relay-ssh`) обязателен; повторный `autostart install` без параметров сохраняет
прежнюю конфигурацию и только обновляет бинарник. Скачанный бинарник можно удалить: installer
копирует его под content-addressed именем, поэтому повторный `install` новой
версией безопасно переключает следующий запуск на обновление. Identity и relay trust
store остаются в обычном пользовательском config-каталоге и при обновлении не
пересоздаются. Pairing сначала выполняется обычным запуском connector;
одноразовые `--pairing-code` и `--accept-new-relay-certificate` сохранять в
автозапуск запрещено.

На Windows создаётся задача `Ariadne Connector` с logon type
`InteractiveToken`, обычными правами текущего пользователя и без credentials.
На Linux создаётся и включается user unit
`~/.config/systemd/user/ariadne-connector.service`. Оба варианта начинают
действовать при следующем входе пользователя; `install` намеренно не запускает
второй connector рядом с уже работающим вручную экземпляром. Windows-логи
пишутся в `%LOCALAPPDATA%\Ariadne\logs\connector.log`, Linux-логи доступны через
`journalctl --user -u ariadne-connector`.

Android-сборка в Termux создаёт скрипт
`~/.termux/boot/20-ariadne-connector`. Для его выполнения нужно установить
Termux:Boot, один раз открыть приложение и исключить Termux из нежелательных
ограничений фоновой работы. Постоянный wake lock автоматически не включается.
Termux-логи находятся в `~/.cache/ariadne/connector.log`.

`--ssh-address` влияет только на опциональный `ari proxy`; для `ari shell` этот адрес не используется, поэтому флаг можно не указывать.

Проверка registry и структурированного exec:

```bash
./bin/ari nodes
./bin/ari claim NODE_ID phone
./bin/ari revoke NODE_ID
./bin/ari exec --command 'uname -a | sed -n "1p"' phone
./bin/ari exec phone -- uname -a
./bin/ari exec --timeout 5s --cwd /tmp phone -- pwd
./bin/ari upload phone ./artifact.bin /tmp/artifact.bin
./bin/ari download phone /tmp/result.bin ./result.bin
./bin/ari job start --command 'task build >build.log 2>&1' buildbox
./bin/ari job list buildbox
./bin/ari job read buildbox JOB_ID
./bin/ari shell phone
```

## MCP и skill для агентов

`ariadne-mcp` предоставляет management plane как локальный stdio MCP без shell-вызовов `ari`:

- `ariadne_nodes` — живые ноды, platform и статус доверенного alias;
- `ariadne_claim` — привязка alias к точному `node_id`;
- `ariadne_revoke` — немедленный отзыв identity и освобождение alias;
- `ariadne_exec` — командная строка через нативный shell узла, опциональный точный `argv`, remote `cwd`, timeout и структурированные stdout/stderr/exit code.
- `ariadne_file_upload` / `ariadne_file_download` — path-to-path streaming между MCP host и узлом без передачи file bytes через контекст модели.
- `ariadne_job_start/list/status/read/cancel/remove` — connector-owned фоновые задачи с ограниченным spool и курсорным чтением stdout/stderr.

MCP запускается рядом с relay, читает тот же локальный management token и не передаёт его connector. Интерактивный shell не включён в MCP: stdio занят протоколом, а агентские операции используют завершённые exec-запросы. Для обычной работы агент передаёт `command`; `argv` нужен только для точного запуска без интерпретации shell.

Файловые tools публикуют destination только после полной передачи и взаимной проверки размера и SHA-256. По умолчанию существующий путь не заменяется; `overwrite` включает атомарную замену. Connector объявляет эту возможность как capability `file-transfer.v1`, поэтому relay не отправляет новый stream старым connector.

Фоновая задача продолжает выполняться при обрыве и переподключении node transport: процесс и stdout/stderr принадлежат долгоживущему процессу connector, а не одной relay-сессии. В первой версии реестр не сохраняется на диск: остановка или перезапуск connector отменяет выполняющиеся задачи и удаляет доступ к накопленному выводу. По умолчанию одновременно работают до 4 задач, на каждый поток вывода сохраняется до 16 MiB, завершённые задачи удерживаются до 24 часов (не более 64 записей).

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

Alias, сообщённый новым connector, отображается в `ari nodes` с суффиксом `?` и не используется как target. Управляющая сторона должна один раз выполнить `ari claim NODE_ID ALIAS`; claim записывается в постоянный registry relay, применяется после reconnect и перезапуска relay и делает alias доступным для `shell`/`exec`. `ari revoke NODE_ID` немедленно отключает точную identity, запрещает ей повторную регистрацию и освобождает alias.

По умолчанию registry находится в `$XDG_CONFIG_HOME/ariadne/node-registry.json` (обычно `~/.config/ariadne/node-registry.json`); путь меняется флагом relay `--registry-file`. Это versioned JSON schema v1 с файлом `0600`. Каждая транзакция сначала сохраняет предыдущее целое состояние в `.bak`, затем атомарно заменяет основной файл. Если основной файл отсутствует, relay автоматически восстанавливает последнюю резервную копию. Повреждённый файл или неизвестная версия schema приводят к fail-closed запуску: следует остановить relay, сохранить оба файла для разбора и явно восстановить проверенный `.bak`. Отсутствующий registry создаётся автоматически; прежние in-memory claims перенести невозможно, их нужно подтвердить один раз после обновления.

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

Relay запускает два независимых HTTP server. Management plane слушает `127.0.0.1:8088`, connector-facing node plane по умолчанию — `127.0.0.1:14771`. Если через роутер уже доступен SSH, node port пробрасывать не требуется: внешний connector сам поднимает ограниченный tunnel через `breakglass`:

```bash
ariadne-connector \
  --relay-ssh breakglass@relay-host:22061 \
  --alias phone
```

SSH port после двоеточия необязателен; без него OpenSSH использует config или порт `22`. OpenSSH запрашивает временный break-glass пароль, создаёт local forward до `127.0.0.1:14771` на relay и остаётся дочерним процессом connector. После истечения password TTL установленный tunnel продолжает работать. Если SSH-соединение оборвётся, connector завершится: для нового входа нужно снова открыть break-glass окно и перезапустить connector.

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
на одном номере порта. Первое знакомство выполняется через короткоживущий
pairing-код и PAKE, по схеме commissioning в Matter. После PAKE connector
сохраняет аутентифицированный отпечаток TLS-сертификата для `host:port`,
закрывает временный сеанс и подключается заново с жёсткой проверкой pin. Для
короткой записи bare host и `quic://HOST` конкурентно пробуют порты `14771`,
`23771`, `47471`; первый порт является рекомендуемым, последний оставлен для
совместимости. `HOST:PORT` и `quic://HOST:PORT` задают ровно один endpoint.
Полный HTTP(S)/WS(S) URL переопределяет transport и не включает discovery:

```bash
./bin/ariadne-relay \
  --management-listen 127.0.0.1:8088 \
  --node-listen 192.168.0.61:14771 \
  --node-loopback-listen 127.0.0.1:14771 \
  --node-quic-listen 192.168.0.61:14771 \
  --node-tls-cert /path/fullchain.pem \
  --node-tls-key /path/privkey.pem

# На машине relay; команда использует закрытый management plane.
ari pair
# pairing code: 12345678

./bin/ariadne-connector \
  --relay relay.example \
  --pairing-code 12345678 \
  --alias workstation
```

Trust store находится в `~/.config/ariadne/known_relays` и создаётся с правами
`0600`. Pairing-код содержит восемь цифр, действует по умолчанию пять минут,
допускает пять PAKE-попыток и расходуется после первого успешного commissioning.
`ari pair` печатает код без разделителя; connector также принимает запись с
необязательным дефисом после четвёртой цифры. Relay хранит в памяти
OPAQUE-verifier, а не сам код. Лимиты меняются флагами relay `--pairing-ttl` и
`--max-pairing-attempts`. Успешный PAKE закрепляет TLS identity relay на
connector и криптографически связывает commissioning с Ed25519 identity
connector. Регистрация identity и management claim alias остаются отдельным
процессом. Для SSH tunnel pairing не нужен: relay уже аутентифицируется
системным OpenSSH.

QUIC и WSS на одном `host:port` используют одну запись. Если relay предъявил
другой сертификат, connector блокирует соединение. Рекомендуемый способ
ротации — открыть новое окно `ari pair` и повторить commissioning с
`--pairing-code`. Для аварийной ручной процедуры сохранена одноразовая замена:

```bash
./bin/ariadne-connector \
  --relay relay.example \
  --accept-new-relay-certificate \
  --alias workstation
```

Для unattended provisioning с заранее известной identity сохранён
`--relay-cert-pin sha256:HEX_DIGEST`; при его наличии PAKE и trust store не
используются. Путь store можно изменить через `--known-relays-file`.

Для автоматического fallback нужно пробросить на relay UDP и TCP одного
выбранного порта. Для адреса без порта connector параллельно проверяет все три
кандидата; внутри каждого кандидата сначала используется QUIC, а через четыре
секунды — `https://HOST:PORT/v1/connect`. Успешный endpoint закрепляется до
перезапуска connector. Fallback можно отключить флагом
`--relay-fallback none` или заменить отдельным URL. Явный fallback URL
запрашивается только один раз. Management plane через роутер не публикуется.
Дополнительный `--node-loopback-listen` сохраняет plaintext endpoint только на
loopback для режима `--relay-ssh`; наружу он не доступен.

При необходимости разового внешнего управления `ari` может создать вспомогательный tunnel:

```bash
ari --relay-ssh breakglass@relay-host nodes
ari --relay-ssh breakglass@relay-host shell phone
```

Обе программы используют системный OpenSSH и выбирают свободный loopback-порт. Ручной `ssh -L` также поддерживается.

У connector нет bootstrap bearer token. Публичный node plane принимает новые
self-authenticated Ed25519 identities, но не содержит управляющих endpoints;
pairing защищает первое доверие connector к relay, а человекочитаемый alias
требует отдельного management claim. QUIC
использует TLS 1.3, ALPN `ariadne/1`, отключённый 0-RTT и keepalive; при разрыве
connector переподключается с той же identity. Отдельный management bearer token
защищает `pair`, `nodes`, `claim`, `revoke`, `exec` и stream endpoints даже на
loopback. Management plane по умолчанию разрешено привязать только к loopback.
Флаги `--allow-insecure-management-listen`, `--allow-insecure-node-listen` и
`--allow-insecure-relay` предназначены для явных plaintext-экспериментов.

## Текущие ограничения

- публичный node plane по-прежнему допускает новые self-authenticated Ed25519 identities; pairing защищает первое доверие connector к relay, а alias требует отдельного management claim;
- identity и aliases сохраняются, но presence и connector-owned jobs остаются только в памяти соответствующих процессов;
- одна relay-инстанция без распределённого presence;
- нет namespace, ACL, approvals, внутренней CA и mTLS;
- `exec` не является sandbox: команда получает права OS-пользователя connector;
- connector, `exec` и встроенный shell пока разделяют один OS UID;
- неподтверждённые aliases являются только метаданными и не используются для lookup;
- terminal I/O не записывается, SSH-содержимое relay не расшифровывает.

Исходный архитектурный набросок находится в [docs/architecture.md](docs/architecture.md), текущий wire-протокол — в [docs/protocol.md](docs/protocol.md).

## Дополнительные инструменты

В репозитории также находится независимая серверная утилита `ssh-breakglass`: она позволяет с доверенного телефона на короткое время включить случайный пароль для отдельного SSH-пользователя и автоматически блокирует его systemd-таймером. Установка и модель безопасности описаны в [docs/ssh-breakglass.md](docs/ssh-breakglass.md).
