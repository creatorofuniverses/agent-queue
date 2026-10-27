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
- если linger выключен, печатает подсказку: без `loginctl enable-linger $USER`
  демон и все задачи останавливаются при выходе из последней сессии.

Перед установкой проверьте `pool_memory` в примере конфига: значение по
умолчанию `16G` должно быть не больше `MemTotal` минус 10 % резерва, иначе демон
не запустится (`CONFIG_INVALID`).

Каталоги переопределяются переменными make: `BINDIR`, `UNITDIR`, `CONFDIR`,
`STATEDIR`, а `SYSTEMCTL` (по умолчанию `systemctl --user`) — например,
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

## Восстановление

- Если демон упал или был перезапущен, задачи продолжают работать сами; при
  старте демон сверяет БД с systemd и забирает результаты завершившихся задач
  (их записывает `ExecStopPost` unit задачи в `jobs/<id>/result.json`).
  Задача, чей unit исчез без результата, получает `LOST`.
- `systemd` перезапускает упавший демон сам (`Restart=on-failure`).
- Пока демон восстанавливается, новые задачи не стартуют.

## Ограничения текущей версии

- Нет резервирования ресурсов заранее и нет backfill: если задача в голове
  очереди не помещается, задачи за ней тоже ждут.
- Нет уведомлений о завершении — только `aq wait` / `aq logs --follow`.
- Нет ретенции логов: старые `jobs/<id>/` не удаляются автоматически.
- Резервы, уведомления и ретенция появятся в следующих планах (2–4).
- Только Linux с systemd и cgroup v2, только пользовательский режим.

## Разработка

```sh
make build              # bin/aq
make test               # go test -race ./...
make test-integration   # go test -tags integration -p 1 ./... (реальный systemd)
```

Интеграционные и e2e-тесты (`test/e2e/`) запускают настоящие units в отдельных
слайсах `aqtest.slice` и `aqe2e.slice` с пулом 1 GiB и после себя их
останавливают; установленный `aq.service` им не нужен.
