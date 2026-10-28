# aq — локальный диспетчер ресурсов

`aq` ставит тяжёлые команды (тесты, сборки, обучение) в очередь к общему пулу
памяти и CPU и запускает каждую в отдельном transient unit systemd с жёсткими
лимитами cgroup v2. Несколько агентов, одновременно запустивших `pytest -n 16`,
больше не вешают машину: задачи ждут своей очереди, а задача, превысившая свой
лимит, получает OOM внутри собственной cgroup, не задевая хост.

Состав:

- один бинарник `aq`: демон (`aq daemon`) и клиентские команды;
- демон работает как пользовательский сервис `aq.service` и общается с
  клиентами через Unix-сокет `$XDG_RUNTIME_DIR/agent-queue/aq.sock`;
- задачи запускаются в `aq.slice` (лимит памяти слайса = `pool_memory`),
  каждая — unit `aq-<id>.service` с `MemoryMax`, `MemorySwapMax=0`,
  `CPUQuota`, `RuntimeMaxSec`;
- состояние: `~/.local/state/agent-queue/` (`aq.db`, логи задач в `jobs/<id>/`),
  конфиг: `~/.config/agent-queue/config.toml`.

## Требования

- Linux с cgroup v2 и делегированными пользователю контроллерами `memory`, `cpu`, `pids`;
- systemd ≥ 254 (пользовательский менеджер);
- Go 1.26 для сборки.

## Установка

```sh
make install
```

`make install` собирает `bin/aq` и:

- кладёт бинарник в `~/.local/bin/aq`;
- кладёт `aq.service` и `aq.slice` в `~/.config/systemd/user/`;
- копирует `deploy/config.example.toml` в `~/.config/agent-queue/config.toml`,
  только если конфига ещё нет (существующий не трогается);
- выполняет `systemctl --user daemon-reload`, включает `aq.service` и
  (пере)запускает его — повторный `make install` обновляет работающий демон;
- копирует скилл `deploy/skills/aq` в `~/.claude/skills/aq` и `~/.codex/skills/aq`
  (пути меняются через `SKILLDIRS`); хуки подключаются отдельно;
- если linger выключен, печатает подсказку: без `loginctl enable-linger $USER`
  демон и все задачи останавливаются при выходе из последней сессии.

Перед установкой проверьте `pool_memory` в примере конфига: значение по
умолчанию `16G` должно быть не больше `MemTotal` минус 10 % резерва, иначе демон
не запустится (`CONFIG_INVALID`).

Каталоги переопределяются переменными make: `BINDIR`, `UNITDIR`, `CONFDIR`,
`STATEDIR`, `SKILLDIRS`, а `SYSTEMCTL` (по умолчанию `systemctl --user`) — например,
`make install SYSTEMCTL=true` только раскладывает файлы.

Проверка после установки:

```sh
aq doctor
systemctl --user status aq.service
```

Все строки `aq doctor` должны быть `ok`. Если запустить `aq doctor` до первого
старта демона, он честно покажет `SLICE_MISSING`, `KILL_DROPIN_MISSING` и
`DAEMON_UNAVAILABLE`: `aq.slice` и runtime drop-in
`$XDG_RUNTIME_DIR/systemd/user/aq-.service.d/99-aq-kill.conf` создаёт сам демон
при старте. После `systemctl --user start aq.service` эти проверки проходят.

Удаление:

```sh
make uninstall   # останавливает и выключает сервис, удаляет бинарник и units
make purge       # то же плюс удаляет ~/.local/state/agent-queue (БД и логи)
```

`make uninstall` сохраняет БД, логи и конфиг. `make purge` удаляет состояние,
но конфиг тоже оставляет.

## Запуск задач

```sh
aq submit --memory 4G --cpus 2 --timeout 30m -- \
  bash -lc '/path/.venv/bin/python -m pytest -n "$AQ_CPUS"'
```

- `--memory` (обязательно) — жёсткий лимит памяти задачи: `4G`, `512M`,
  `1024K`, `2T` (двоичные единицы), голое число — MiB; минимум `64M`.
- `--timeout` (обязательно) — максимальное время работы, например `30m`.
- `--cpus` — сколько CPU пула занять (по умолчанию 1); задаёт `CPUQuota`.
- `--priority low|normal|high`, `--name`, `--key` (идемпотентность: повторный
  `submit` с тем же ключом возвращает ту же задачу), `--session`, `--env K=V`,
  `--expected` (ожидаемое потребление, по умолчанию = `--memory`), `--json`.

Команда передаётся как argv, без неявного shell, и systemd не раскрывает в ней
переменные. Поэтому `$AQ_CPUS` нужно раскрывать внутри задачи: запуск через
`bash -lc '…'` в одинарных кавычках оставляет `$AQ_CPUS` нетронутым для вашего
shell, и раскрывает его уже `bash` внутри unit, где сервис выставил
`AQ_CPUS=<--cpus>`. Без `bash -lc` переменную раскрыл бы ваш shell до запуска
`aq` (и получил бы пустую строку). Кроме `AQ_CPUS` задача получает `AQ_JOB_ID`,
собственные `TMPDIR`/`TMP`/`TEMP`, а из вызывающего окружения — `PATH`, `HOME`,
`LANG`, `LC_*`, `VIRTUAL_ENV`.

`aq submit` печатает id задачи и сразу возвращает управление.

## Наблюдение и ожидание

```sh
aq list              # активные задачи: состояние, память, ожидание, BLOCKED_BY
aq list --all        # включая завершённые
aq status <id>       # подробности: состояние, причина, пик памяти, код выхода
aq result <id>       # то же плюс пути к stdout.log/stderr.log
aq logs <id> [--stream stderr] [--tail 100] [--follow]
aq wait <id>         # ждёт завершения, печатает итог и выходит с кодом:
```

| Код `aq wait` | Значение |
|---|---|
| 0 | `SUCCEEDED` |
| 1 | `COMMAND_FAILED` (команда вышла с ненулевым кодом; он в `aq status`) |
| 2 | `OOM`, `OOM_GROUP`, `OOM_HOST_GUARD` |
| 3 | `TIMEOUT` |
| 4 | `CANCELLED` |
| 5 | инфраструктура: `LAUNCH_FAILED`, `LOST`, `STORAGE_FULL`, `REJECTED` |
| 6 | резерв больше не удерживается (см. «Резервы») |
| 64 | ошибка использования |
| 69 | демон недоступен |

Типичный сценарий агента:

```sh
id=$(aq submit --memory 4G --cpus 2 --timeout 30m -- make test)
aq wait "$id"; code=$?
aq logs "$id" --tail 50
```

У всех команд просмотра есть `--json` для разбора программами.

## Диагностика: `aq doctor`

`aq doctor` проверяет cgroup v2, делегирование контроллеров, наличие слайса,
реальное применение `MemoryMax` (пробный unit на 64M), конфиг, свободное место,
доступность демона, версию systemd и runtime drop-in остановки задач. Каждая
ошибка помечена кодом (`CGROUP_V2_MISSING`, `CONTROLLER_MISSING`,
`SLICE_MISSING`, `PROBE_FAILED`, `CONFIG_INVALID`, `DISK_LOW`,
`DAEMON_UNAVAILABLE`, `SYSTEMD_TOO_OLD`, `KILL_DROPIN_MISSING`) и подсказкой.

## Изменение пула

Отредактируйте `~/.config/agent-queue/config.toml` (`pool_memory`, `pool_cpus`,
`reserve_floor_percent` и т. д.; все ключи с описанием — в
`deploy/config.example.toml`) и перезапустите демон:

```sh
systemctl --user restart aq.service
```

Работающие задачи при перезапуске демона не прерываются: это отдельные units
systemd, и новый демон подхватывает их состояние и результаты.

## OOM и другие сбои

Если задача превысила `--memory`, ядро убивает её внутри её cgroup, а `aq wait`
выходит с кодом 2. Подробности:

```sh
aq status <id>                    # причина OOM, пик памяти (memory peak)
journalctl --user -u aq-<id>      # журнал unit задачи (systemd: oom-kill и т. п.)
aq logs <id> --stream stderr
```

`OOM_GROUP` — задачу убил лимит пула (`aq.slice`), `OOM_HOST_GUARD` — защитник
хоста, временно снизивший потолок слайса из-за нехватки памяти на машине.
Увеличьте `--memory` или уменьшите параллелизм команды.

## Отмена

```sh
aq cancel <id> [--reason "..."]
```

Задача в очереди снимается сразу. У работающей останавливается весь unit:
systemd шлёт SIGTERM, через `cancel_grace` (по умолчанию 10 с) — SIGKILL всем
процессам cgroup, включая ушедших в `setsid`/демонизированных потомков.

## Резервы

Резерв нужен, когда память занимает не сама команда, а то, что живёт дольше неё
или вне её cgroup: compose-стенд (база, брокер, сервисы приложения) или
контейнеры, которые тесты поднимают через testcontainers. Контейнеры работают в
cgroup dockerd, лимиты задачи на них не действуют, и без резерва пул о них не
знает. Резерв заранее занимает в пуле память (и, если нужно, CPU) стенда на
заданный срок, а также бюджет для задач, которые будут работать со стендом.

```sh
cat > teardown.sh <<'EOF'
docker compose -p myapp-e2e down -v --remove-orphans
EOF

out=$(aq reserve --memory 6G --cpus 2 --task-memory 4G --task-cpus 4 \
  --ttl 2h --teardown teardown.sh --docker-project myapp-e2e)
# out: "<id> GRANTED" или "<id> QUEUED position=… blocked_by=…"
id=${out%% *}
aq wait "$id"                      # ждёт выдачи; код 0 = GRANTED/EXPIRING
docker compose -p myapp-e2e up -d  # стенд поднимаем только после выдачи
```

- `--memory` (обязательно) — память стенда; `--cpus` — CPU стенда (по
  умолчанию 0).
- `--task-memory` и `--task-cpus` — бюджет вложенных задач (`submit --within`),
  задаются только вместе: минимум `64M` и 1 CPU.
- `--ttl` (обязательно) — срок резерва, отсчитывается от выдачи; не больше
  `max_ttl` (24 ч).
- `--teardown` (обязательно) — файл скрипта снятия (до 64 KiB).
- `--docker-project` — compose-проект стенда. Демон раз в 5 с замеряет через
  `/var/run/docker.sock` память контейнеров с меткой
  `com.docker.compose.project=<project>` и считает ещё не выбранной только
  разницу между заявленным и замеренным. Без `--docker-project` весь `--memory`
  стенда всё время жизни резерва считается невыбранным: это надёжно, но память
  недоиспользуется. Один проект может принадлежать только одному незавершённому
  резерву (пока тот не `TORN_DOWN`, `RELEASED` или `CANCELLED`). Если стенд
  превысил заявленное, записывается уведомление.
- `--name`, `--key`, `--session`, `--priority`, `--env K=V`, `--json` — как у
  `submit`.

Стенд и бюджет задач вместе занимают пул, пока резерв не освобождён. Резерв
встаёт в очередь так же, как задача, а `aq reserve` отвечает после ближайшего
прохода допуска: сразу `GRANTED`, если влез, иначе `QUEUED` с позицией и
причиной ожидания.

### Скрипт снятия

При `reserve` клиент передаёт содержимое скрипта, и демон хранит его копию в
`jobs/<id>/teardown.sh`. По истечении срока демон запускает `bash <копия>`
отдельным unit `aq-teardown-<id>.service` в слайсе `aqtd.slice`: вне пула, с
лимитами `teardown_memory`, 1 CPU и `teardown_timeout` (по умолчанию 5 мин).
Скрипт должен быть:

- **самодостаточным** — не зависеть от файлов рабочего каталога: к моменту
  снятия worktree может быть удалён. cwd скрипта — исходный каталог резерва,
  если он ещё существует, иначе пустой служебный `jobs/<id>/teardown-<n>/cwd`;
  исходный путь передаётся в `AQ_ORIGINAL_CWD`, id резерва — в
  `AQ_RESERVATION_ID`;
- **идемпотентным** — повторный запуск безопасен: `docker compose -p <project>
  down`, а не «удалить то, что я создал».

Окружение скрипта захватывается при `reserve`: то же, что у `submit`, плюс
`DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG` и `--env`.

### Задачи на стенде: `submit --within`

pytest против стенда, как и тесты, которые сами поднимают контейнеры через
testcontainers, запускаются внутри резерва:

```sh
aq submit --within "$id" --memory 3G --cpus 4 --timeout 30m -- \
  bash -lc 'pytest -n "$AQ_CPUS" tests/e2e'
```

Вложенная задача не проходит повторный допуск в пул и не стоит в очереди: она
стартует сразу, в бюджете резерва. Если сумма `expected` неоконченных вложенных
задач вместе с новой больше `--task-memory` или сумма их CPU больше
`--task-cpus`, `submit` сразу отказывает (`REJECTED`, «task budget», код 5).
Так два агента, получившие по резерву, не блокируют друг друга. Контейнеры,
которые поднимает сама задача, живут в cgroup dockerd, а не задачи: заложите их
память в `--memory` резерва (замер `--docker-project` видит только контейнеры
compose-проекта), и пусть скрипт снятия убирает и их.

### Продление и освобождение

```sh
aq extend "$id" --ttl 1h   # срок истечёт не раньше чем через 1 ч от сейчас
aq release "$id"           # стенд погашен самим агентом: освободить без скрипта
```

`extend` не прибавляет срок, а задаёт «не раньше чем сейчас + D», поэтому
повтор безопасен; продлить можно в `GRANTED` и `EXPIRING`. `release`
освобождает выданный резерв, если в нём нет неоконченных вложенных задач, а
резерв из очереди отменяет (`CANCELLED`). `aq cancel` для резерва действует
только в очереди.

### Истечение и неудачное снятие

- За `expiry_warning` (15 мин) до конца срока резерв переходит в `EXPIRING`, и
  записывается предупреждение «продли или будет снят».
- По истечении срока резерв переходит в `TEARING_DOWN` и запускается скрипт
  снятия (одновременно не больше `teardown_concurrency`; остальные ждут слота в
  `EXPIRING`). Код 0 → `TORN_DOWN`, память возвращается в пул.
- Если в резерве ещё работают вложенные задачи, он не снимается: срок
  сдвигается до их дедлайна (`started_at + timeout + 2 × cancel_grace`),
  `aq status` показывает `ttl held: yes`, новые `--within` отклоняются.
  Как только последняя вложенная задача закончилась, резерв снимается сразу,
  не дожидаясь сдвинутого срока.
- Ненулевой код, таймаут или OOM скрипта → `TEARDOWN_FAILED`; unit снятия
  пропал без результата → `TEARDOWN_UNKNOWN`. В обоих случаях резерв
  **остаётся в учёте пула**, автоматических повторов нет. Разберитесь со
  стендом (`aq logs <id> --stream stderr` показывает логи последней попытки,
  все попытки лежат в `jobs/<id>/teardown-<n>/`) и выберите:

```sh
aq teardown --retry "$id"  # запустить скрипт снятия ещё раз
aq release --force "$id"   # стенд убран вручную: освободить без скрипта
```

Падение или перезапуск демона снятие не прерывают: unit работает сам, а новый
демон забирает его результат.

Коды `aq wait` для резерва (возвращается, как только резерв вышел из очереди):

| Код | Значение |
|---|---|
| 0 | `GRANTED`, `EXPIRING` — резерв выдан |
| 4 | `CANCELLED` |
| 5 | `REJECTED` |
| 6 | резерв больше не удерживается: `RELEASED`, `TEARING_DOWN`, `TORN_DOWN`, `TEARDOWN_FAILED`, `TEARDOWN_UNKNOWN` |

Уведомления о выдаче из очереди, истечении и превышении заявки идут владельцу;
истечение, удержание задачами, итог и принудительное снятие — оператору (см. ниже).

## Уведомления

Клиент обнаруживает сессию по настроенному адаптеру с непустой `identity_env`:
Codex (`CODEX_THREAD_ID`, push через `codex queue`) или Claude
(`CLAUDE_CODE_SESSION_ID`, client-wait через фоновый `aq wait`). Если непусты
переменные нескольких адаптеров (сессия запущена из шелла другого агента и
унаследовала его переменную), aq не угадывает: submit/reserve завершаются с кодом
64, MCP требует явные `client` и `session_id`. `--client NAME --session S` задают их явно; `--no-notify` у submit/reserve
отключает только уведомления сессии. Без обнаруженной сессии CLI работает как
`cli`. Указанный push-адаптер без сессии — ошибка; отсутствующая в PATH демона
команда доставки отклоняет заявку до сохранения. PATH сервиса включает
`%h/.local/bin:/usr/local/bin:/usr/bin:/bin`.

Новый push-клиент добавляется без правки ядра:

```toml
[[agents]]
name = "pi"
identity_env = "PI_SESSION"
delivery = "push"
command = ["pi", "send", "{session}", "{text}"]
```

Если есть хотя бы один `[[agents]]`, список **заменяет** умолчания целиком.
Команда — argv без shell; также поддерживаются `{event_id}`, `{item_id}`,
`{kind}`. `delivery = "client-wait"` требует пустую command.

После submit/reserve Claude запускает `aq wait ID` с `run_in_background` и
заканчивает ход; Codex получает пинг автоматически. `aq wait RES_ID --until
expiring` ждёт предупреждения об истечении или превышении заявки стенда.
Если он вернулся с `GRANTED`/`stand_over_declared`, обработайте и подтвердите
событие, затем запустите этот waiter снова фоном для будущего истечения.

```sh
aq list --mine             # свои активные + завершённые с неподтверждёнными событиями
aq list --session SESSION  # явный фильтр
aq result ITEM_ID
aq ack EVENT_ID            # после записи результата в чек-пойнт
```

Событие атомарно сохраняется вместе с изменением состояния. Push/webhook
повторяются с backoff и jitter до успешной доставки (sent); sent без ack
повторно не будит сессию. Падение демона может дать дубль с тем же event_id:
сверяйте квитанцию и пропускайте уже обработанные события. Ack до захвата
события отменяет доставку, после захвата пинг ещё может прийти. `aq wait`
показывает доставленные event IDs; прерванный waiter можно запустить снова.

Операторские тревоги включаются через конфиг:

```toml
[webhook]
url = "https://hooks.example.org/aq"
token_file = "/absolute/path/to/token"
timeout = "10s"
```

POST с `Content-Type: application/json`, `Idempotency-Key: EVENT_ID`, при токене
`Authorization: Bearer TOKEN`. Токен читается заново при каждой попытке.
Тело содержит `event_id, kind, audience, item_id, item_kind, item_name, state,
reason, project, session_id, client_type, detail, text, created_at` (RFC3339).
Значений env, скрипта снятия и логов в уведомлениях нет; текст ограничен 8 KiB.
Без URL операторские события сохраняются skipped. Через `give_up_after`
неудачная доставка становится failed; сбой session-доставки создаёт
`delivery_failed` оператору, сбой операторской — только логируется.
Пороги и таймауты — в `[notify]` примера конфига. `aq doctor` показывает
`ADAPTER_UNAVAILABLE`, `WEBHOOK_FAILING`, а также отключённый webhook.

## MCP

Один бинарник работает как stdio MCP-сервер; демон должен быть запущен отдельно.
Примеры подключения (выполните самостоятельно):

```sh
claude mcp add --transport stdio aq -- /home/USER/.local/bin/aq mcp
```

В конфиге Codex:

```toml
[mcp_servers.aq]
command = "/home/USER/.local/bin/aq"
args = ["mcp"]
env_vars = ["XDG_RUNTIME_DIR"]
```

Либо задайте `[mcp_servers.aq.env] AQ_RUNTIME_DIR = "/run/user/1000/agent-queue"`
(для своего UID), при нестандартных путях также AQ_STATE_DIR и AQ_CONFIG.
Codex 0.164 фильтрует окружение MCP: сессия автоматически не передаётся.
Для `queue_submit`/`queue_reserve` передавайте **client и session_id** текущей
сессии явно, либо осознанно `no_notify: true`. Неизвестная идентичность —
ошибка, а не молчаливое отключение уведомлений. Если launcher передал правильную
переменную сессии в MCP env явно, aq обнаружит её.
[Матрица клиентов и фактические проверки](docs/notes/aq-clients.md).

Инструменты: queue_submit, queue_reserve, queue_extend, queue_release,
queue_cancel, queue_status, queue_result, queue_list, queue_ack, queue_logs.
`cwd` и `teardown_file` абсолютные; queue_logs: tail 1..2000 (по умолчанию 200),
без follow. MCP не будит простаивающую сессию: Claude всё ещё запускает
`aq wait ID` фоном после постановки через MCP; Codex получает daemon push.

## Хук

`aq hook pretooluse` мягко напоминает о тяжёлой команде вне aq: pytest с `-n`,
compose up/run, docker run. Он не обращается к демону, не блокирует и при ошибке
молчит. Шаблоны — `[hook].patterns`. В счётчик hook-hits.jsonl пишутся только
время, индекс шаблона и клиент, без текста команды; ротация после 1 MiB.

Установка не меняет настройки хуков. Вручную объедините нужный фрагмент
[Claude](deploy/hooks/claude-settings.json) или [Codex](deploy/hooks/codex-hooks.json)
с настройками клиента (не перезаписывайте существующие hooks). В обоих matcher
`Bash`; Codex требует доверить новое определение через `/hooks`.
Скилл [aq](deploy/skills/aq/SKILL.md) описывает квитанции, ack и снятие резервов.

## Восстановление

- Если демон упал или был перезапущен, задачи продолжают работать сами; при
  старте демон сверяет БД с systemd и забирает результаты завершившихся задач
  (их записывает `ExecStopPost` unit задачи в `jobs/<id>/result.json`).
  Задача, чей unit исчез без результата, получает `LOST`.
- `systemd` перезапускает упавший демон сам (`Restart=on-failure`).
- Пока демон восстанавливается, новые задачи не стартуют.

## Ограничения текущей версии

- Нет backfill: если задача или резерв в голове очереди не помещается, всё
  за ним тоже ждёт.
- Смерть сессии-владельца резерва не определяется; автоматического снятия по
  этому признаку нет.
- Нет ретенции логов: старые `jobs/<id>/` не удаляются автоматически.
- Профили, backfill, оценки, статистика и ретенция — следующий план (4).
- Только Linux с systemd и cgroup v2, только пользовательский режим.

## Разработка

```sh
make build              # bin/aq
make test               # go test -race ./...
make test-integration   # go test -tags integration -p 1 ./... (реальный systemd)
```

Интеграционные и e2e-тесты (`test/e2e/`) запускают настоящие units в отдельных
слайсах `aqtest.slice` и `aqe2e.slice` с пулом 1 GiB (снятия резервов — в
`aqtesttd.slice` и `aqe2etd.slice`) и после себя их останавливают;
установленный `aq.service` им не нужен.
