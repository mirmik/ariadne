# Временный пароль SSH с управлением с телефона

`ssh-breakglass` открывает короткое окно входа по паролю для отдельного SSH-пользователя. Постоянный ключ остаётся только на телефоне: с него администратор включает окно, а на чужом компьютере вводит случайный быстро протухающий пароль.

## Установка на сервер

Нужны Linux, OpenSSH, systemd и права root. Соберите и установите бинарник:

```bash
task build
sudo ./scripts/install-ssh-breakglass \
  --admin-user trusted-admin \
  --login-as working-user
```

Замените `trusted-admin` на серверную учётную запись, ключ которой находится на телефоне, а `working-user` — на учётную запись, в которой находятся нужные файлы. Если это один пользователь, `--login-as` можно опустить: он по умолчанию принимает значение `--admin-user`.

Скрипт идемпотентно создаёт пользователя `breakglass`, блокирует его пароль, устанавливает бинарник, SSH drop-in, boot unit и ограниченные правила sudoers. Он проверяет синтаксис и эффективную конфигурацию `sshd` до reload; при ошибке восстанавливает прежний drop-in.

Ниже приведены те же действия для ручной установки.

### Ручная установка

```bash
go build -o ssh-breakglass ./cmd/ssh-breakglass
sudo install -o root -g root -m 0755 ssh-breakglass /usr/local/sbin/ssh-breakglass
```

Создайте отдельного пользователя без административных прав и сразу заблокируйте пароль:

```bash
sudo useradd --create-home --shell /bin/bash breakglass
sudo passwd -l breakglass
```

Разрешите пароль только этому пользователю. Установите готовый drop-in, который сортируется раньше глобального `60-key-only-auth.conf`:

```bash
sudo install -o root -g root -m 0644 \
  deploy/ssh-breakglass/00-breakglass.conf \
  /etc/ssh/sshd_config.d/00-breakglass.conf
```

Содержимое файла:

```sshconfig
Match User breakglass
    PasswordAuthentication yes
    KbdInteractiveAuthentication no
    AuthenticationMethods password
    PubkeyAuthentication no
    AllowAgentForwarding no
    AllowTcpForwarding local
    PermitOpen 127.0.0.1:47471 127.0.0.1:8088
    X11Forwarding no
    PermitTunnel no
    PermitUserRC no

Match all
```

Проверьте итоговую конфигурацию до reload. Обе команды должны завершиться успешно:

```bash
sudo sshd -t
sudo /usr/local/sbin/ssh-breakglass check
```

Затем перечитайте конфигурацию. Имя unit зависит от дистрибутива:

```bash
sudo systemctl reload ssh || sudo systemctl reload sshd
```

Проверьте вход во второй сессии, не закрывая существующую административную сессию.

### Автоблокировка после перезагрузки

Transient timer systemd исчезает при перезагрузке. Чтобы аварийный пароль всегда блокировался при старте сервера, установите unit `/etc/systemd/system/ssh-breakglass-lock.service`:

```ini
[Unit]
Description=Lock the SSH break-glass password at boot
Before=ssh.service sshd.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/ssh-breakglass disable

[Install]
WantedBy=multi-user.target
```

Активируйте его и выполните первоначальную блокировку:

```bash
sudo systemctl daemon-reload
sudo systemctl enable ssh-breakglass-lock.service
sudo /usr/local/sbin/ssh-breakglass disable
```

## Использование

С телефона, уже имеющего постоянный ключ:

```bash
ssh server sudo /usr/local/sbin/ssh-breakglass enable --ttl 15m
```

Команда покажет имя пользователя, случайный пароль и точное время закрытия окна. С чужой машины:

```bash
ssh breakglass@server
```

После входа перейдите в рабочую учётную запись:

```bash
sudo -iu working-user
```

Это даёт сессии права и домашний каталог `working-user`, но не предоставляет отдельного root-доступа. Если рабочая учётная запись сама имеет дополнительные права sudo, продолжает действовать её обычная политика sudo.

Аккаунту разрешены только два local forwarding destination Ariadne:

- node plane `127.0.0.1:47471` — основной tunnel внешнего connector;
- management plane `127.0.0.1:8088` — вспомогательный разовый tunnel для `ari`.

Основной сценарий запуска connector на внешней машине:

```bash
ariadne-connector --relay-ssh breakglass@server --alias phone
```

Ручной эквивалент node tunnel:

```bash
ssh -N -T -L 127.0.0.1:17471:127.0.0.1:47471 breakglass@server
ariadne-connector --relay http://127.0.0.1:17471 --alias phone
```

Вспомогательный management tunnel:

```bash
ssh -N -T -L 127.0.0.1:18088:127.0.0.1:8088 breakglass@server
```

Произвольные SSH-туннели остаются запрещены директивой `PermitOpen`.

CLI `ari` может управлять вспомогательным tunnel автоматически:

```bash
ari --relay-ssh breakglass@server nodes
ari --relay-ssh breakglass@server shell TARGET
```

Пароль автоматически блокируется по окончании TTL. Открытая SSH-сессия продолжает работать; новые входы перестают приниматься. Досрочное закрытие и проверка состояния:

```bash
ssh server sudo /usr/local/sbin/ssh-breakglass disable
ssh server sudo /usr/local/sbin/ssh-breakglass status
```

Если не хочется давать телефонному пользователю произвольный `sudo`, добавьте через `visudo` отдельное правило (замените `trusted-admin` на имя его серверной учётной записи):

```sudoers
trusted-admin ALL=(root) NOPASSWD: /usr/local/sbin/ssh-breakglass
```

Утилита намеренно управляет только фиксированным пользователем `breakglass`, поэтому это правило нельзя использовать для смены или блокировки пароля другого аккаунта. `trusted-admin` не должен иметь возможности менять бинарник или каталог `/usr/local/sbin`.

Чтобы разрешить переход из аварийной сессии в рабочую учётную запись без root, установщик создаёт эквивалент такого правила:

```sudoers
breakglass ALL=(working-user) NOPASSWD: ALL
```

Если аварийная сессия должна позволять полное администрирование, root следует выдавать отдельным осознанным правилом. Тогда любой вход в течение открытого окна фактически даёт root:

```sudoers
breakglass ALL=(ALL:ALL) ALL
```

## Ограничения безопасности

- Чужая машина всё ещё может записать содержимое активной сессии или перехватить управление ею.
- Окно ограничивает время повторного входа, но пароль допускает несколько входов внутри TTL.
- Пользователю `breakglass` не следует выдавать неограниченный `sudo`. Нужные административные операции лучше разрешать отдельно.
- Перед вводом пароля на новой машине сверьте fingerprint SSH host key с доверенным значением на телефоне.
