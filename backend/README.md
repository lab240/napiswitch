# napi-switchd

Бекенд управления коммутатором NAPI-Switch (RK3308 + Microchip KSZ9567, Linux DSA).
REST API с JSON для веб-морды. Один бинарник на Go без зависимостей, около 7 МБ на диске
и 7 МБ памяти.

## Что умеет

| Функция | Как сделано | Сохраняется |
|---|---|---|
| Порты: вкл/выкл, описание, скорость/дуплекс, autoneg | `ip link`, `ethtool -s` | networkd (вкл/выкл), демон при старте (остальное) |
| Статус портов: линк, скорость, SQI (качество сигнала), STP-состояние | `ip -j`, `ethtool --json` | — |
| Счётчики MIB по портам, сброс | `ethtool -S` | — (сброс хранится в памяти демона) |
| VLAN 802.1Q: access/trunk, PVID, tagged, VLAN управления | `bridge vlan` | networkd `[BridgeVLAN]` |
| Изоляция портов (protected port), обучение MAC вкл/выкл | `bridge link` | networkd `[Bridge]` |
| MAC-таблица: просмотр, фильтры, очистка, статические записи | `bridge fdb` | networkd `[BridgeFDB]` |
| STP (802.1D): приоритет, таймеры, стоимость/приоритет порта, корень | `ip link … type bridge` | networkd |
| IGMP snooping, querier, группы | `bridge mdb` | networkd |
| Зеркалирование порта (1 сессия, rx/tx/both, только аппаратно) | `tc matchall mirred skip_sw` | демон при старте |
| Применение с подтверждением и автооткатом | демон | — |
| WAN (USB): DHCP или статика (адрес, шлюз, DNS) | networkd | свои файлы networkd |
| LAN (br0): DHCP-клиент или статика; в режиме роутера — адрес шлюза LAN | networkd | свои файлы networkd |
| Режим роутера: NAT, DHCP-сервер (пул, срок аренды, закреплённые адреса), выданные адреса | networkd (`IPMasquerade`, `DHCPServer`) | свои файлы networkd |
| Межсетевой экран WAN (ответы + выбранные службы: ssh, веб, ping), проброс портов WAN → LAN | nftables, таблица `inet napi_switchd` | демон при старте |

Нет в v0.1 (ограничения драйвера ksz9477 или платы): RSTP/MSTP, LAG, ограничение скорости,
ACL, jumbo/MTU (смена MTU переоткрывает trunk0 и вешает канал к свитчу), QoS (4 очереди
есть, будет в следующей версии), диагностика кабеля.

## Как устроено

- **Источник правды — конфигурация демона** (`/etc/napi-switch/config.json`), её редактирует API.
- **Применение** — демон читает текущее состояние ядра, строит план (список команд) и выполняет
  только разницу. Если команда падает, демон возвращает предыдущую конфигурацию.
- **Подтверждение.** `?confirm=N`: если за N секунд не вызвать `/config/confirm`,
  конфигурация откатывается сама. Веб-морда должна вызывать confirm, как только снова
  достучалась до свитча.
- **Сохранение** через systemd-networkd (сеть принадлежит netplan/networkd): демон пишет
  drop-in `50-napi-switchd.conf` рядом с файлами netplan
  (`/etc/systemd/network/10-netplan-br0.netdev.d/`, `…-lanN.network.d/`).
  `netplan generate` их не трогает. Чего networkd не умеет (скорость, описание, зеркало),
  демон применяет при старте.
- **При старте** демон ждёт br0 и порты, применяет сохранённую конфигурацию.
  Без сохранённой конфигурации берёт текущее состояние как есть и ничего не меняет.

### Сеть (L3) и режим роутера

Пока в конфигурации нет раздела `network`, адресами управляет netplan (WAN и br0 по DHCP).
После «взять управление» демон владеет двумя файлами networkd целиком:

- применённая конфигурация — `/run/systemd/network/03-napi-switchd-{wan,lan}.network`;
- сохранённая — `/etc/systemd/network/04-napi-switchd-{wan,lan}.network`.

networkd берёт первый подходящий по имени файл из всех каталогов, поэтому применённый (03) главнее
сохранённого (04), оба главнее netplan (10-netplan-*). После перезагрузки /run пуст — действует
сохранённый. Если удалить оба, снова действует netplan. Наш файл br0 заменяет netplan-овский
вместе с drop-in, поэтому VLAN управления дублируется в нём.

Изменения сети применяются только с подтверждением (`confirm` > 0): ошибка в адресе WAN
отрезает управление, автооткат его вернёт. В режиме роутера:

- DHCP-сервер отвечает на всех портах lan1–lan5 — валидация предупреждает, если на каком-то
  порту есть линк (не подключена ли туда другая сеть);
- подсеть LAN не может пересекаться с подсетью WAN;
- межсетевой экран и проброс портов требуют nftables; правила загружает демон при старте,
  то есть после загрузки есть короткое окно до запуска демона, когда вход с WAN не фильтруется;
- проброс на порт ssh или веб-интерфейса запрещён, если эти службы открыты с WAN.

### Особенности драйвера ksz9477 (учтены в коде)

- `configure_vlan_while_not_filtering = false`: VLAN, добавленные при выключенном 802.1Q,
  не попадают в таблицу свитча. При включении режима (и при старте) демон пересоздаёт VLAN портов.
- При включении 802.1Q на ходу мост не создаёт локальную FDB-запись своего MAC в VLAN
  управления: без неё ответы управлению уходят флудом. Демон добавляет её сам.
- FID = VID mod 128: VLAN, отличающиеся на кратное 128, делят одну аппаратную MAC-таблицу
  (валидация выдаёт предупреждение).
- Во время смены топологии STP ядро показывает сокращённый ageing time; демон его не «чинит».
- Стоимость STP порта после ручной установки нельзя вернуть в «авто» (`stp_cost: 0` = не трогать).

### Временный обход: тактирование RMII (ошибка платы)

**Временно, до перепайки плат.** На прототипе страп R29 тянет RXD6_1 KSZ9567 к GND, и порт 6
работает в RMII «Clock Mode»: свитч берёт 50 МГц от своего кварца и игнорирует вход
REFCLKI6, куда разведена частота процессора (`clock_in_out = output`). Две независимые
частоты на одном канале RMII дают ошибки CRC в обе стороны — до 50 % потерь трафика к плате.

Пока платы не перепаяны (R29 на DVDIO), napi-switchd при старте и затем раз в минуту
ставит бит 2 регистра KSZ 0x6301 (RMII Normal Mode) через одноразовый модуль
`kmod/kszreg` (собирается на плате при `install.sh`, нужны linux-headers). Результат:
0 ошибок CRC, TCP 92,8 Мбит/с. На перепаянной плате бит уже стоит и демон ничего не делает
(`/system` → `cpu_port.rmii.fixed_count: 0`). После перепайки всех плат обход удаляется
целиком: `rmii.go`, `rmii_test.go`, `kmod/`, флаги `-rmii-fix`/`-kszreg-module` и шаг kszreg
в `install.sh`.

## Установка

```
deploy/install.sh root@192.168.30.82            # сборка + копирование, сервис не включается
ssh root@192.168.30.82 systemctl enable --now napi-switchd
ssh root@192.168.30.82 cat /etc/napi-switch/token   # токен API (создаётся при первом запуске)
```

Флаги: `-listen :8080`, `-config`, `-token-file` (пусто = без авторизации), `-bridge br0`,
`-netdir /etc/systemd/network`, `-run-netdir /run/systemd/network`, `-fw-file /run/napi-switch/firewall.nft`,
`-dry-run` (только логировать команды).
Лог — `journalctl -u napi-switchd`, каждая выполняемая команда пишется как `exec: …`.

## Веб-интерфейс

Демон сам раздаёт интерфейс: `http://192.168.30.82:8080/` (файл `web/index.html`, встроен в бинарник).
Вход по токену API; можно открыть сразу ссылкой `http://192.168.30.82:8080/#token=<токен>`
(фрагмент после `#` на сервер не уходит и сразу убирается из адресной строки).

Вкладки: порты (вкл/выкл, описание, скорость, autoneg, обучение, изоляция, STP, счётчики),
VLAN (режим 802.1Q, VLAN управления, матрица U/T), STP, MAC-таблица (фильтры, очистка,
статические записи), мультикаст, зеркалирование, система, журнал. Изменения копятся в
черновике (изменённые поля отмечены), «Что изменится» показывает команды, «Применить» —
с автооткатом через выбранное время, пока не нажать «Подтвердить».

## API

Полное описание с примерами ответов — [API.md](API.md).

Все запросы, кроме `/health`: заголовок `Authorization: Bearer <token>`. Префикс `/api/v1`.

### Состояние (только чтение)

| Запрос | Ответ |
|---|---|
| `GET /health` | `{status, version}` |
| `GET /system` | имя/место/контакт, hostname, модель, ядро, uptime, температуры, адреса, здоровье канала CPU↔свитч (`cpu_port`: CRC/align ошибки RMII) |
| `GET /capabilities` | что поддерживается: скорости портов, VLAN, зеркало, STP… — для построения UI |
| `GET /ports`, `GET /ports/{name}` | линк, скорость, SQI, PVID/tagged, VLAN из ядра, STP, learning, isolated |
| `GET /ports/{name}/counters` | `summary` (байты, unicast/multicast/broadcast, ошибки, discards, pause) + `mib` (все счётчики) |
| `POST /ports/{name}/counters/clear` | сброс счётчиков (в памяти демона) |
| `GET /vlans` | VLAN с участниками (порт, untagged, pvid) и признаком VLAN управления |
| `GET /fdb?port=&vlan=&type=dynamic\|static\|local` | MAC-таблица |
| `DELETE /fdb?port=lan1` | очистить динамические записи (без `port` — все порты) |
| `GET /stp` | bridge/root id, корневой порт, таймеры, состояния портов |
| `GET /multicast` | IGMP snooping/querier, группы (mdb) |
| `GET /mirror` | текущая сессия зеркалирования |
| `GET /events` | журнал изменений (применено/подтверждено/откат) |
| `GET /network` | интерфейсы (WAN-кандидаты и br0: адреса, шлюз, DNS, файл networkd), выданные DHCP-адреса, наличие nftables, `suggested` — конфигурация для «взять управление» |

### Конфигурация

| Запрос | Что делает |
|---|---|
| `GET /config` | `{applied, saved, unsaved_changes, pending}` |
| `PATCH /config?confirm=60&save=1` | JSON merge patch (RFC 7396) поверх текущей; массивы заменяются целиком, `null` удаляет |
| `PUT /config?confirm=60&save=1` | полная замена |
| `POST /config/preview` | проверка + план команд без применения (тело — patch; `?mode=replace` — полная) |
| `POST /config/confirm` | подтвердить изменение (и сохранить, если было `save=1`) |
| `POST /config/rollback` | откатить неподтверждённое изменение сразу |
| `POST /config/save` | сохранить текущую применённую конфигурацию |

`confirm=0` (по умолчанию) — применить без подтверждения. Пока изменение ждёт
подтверждения, новые изменения отклоняются (409).

Коды ошибок: 400 (плохой JSON или неизвестное поле), 401, 404, 409 (ждёт подтверждения
или команда упала: `apply_error` с командой и признаком `restored`), 422 (валидация:
`problems` — все ошибки сразу), 500. Предупреждения (`warnings`) конфигурацию не блокируют.

### Модель конфигурации

```json
{
  "system": {"name": "napi-sw-1", "location": "цех 2, шкаф 3", "contact": ""},
  "bridge": {
    "vlan_aware": true,
    "mgmt_vlan": 1,
    "ageing_time": 300,
    "stp": {"enabled": true, "priority": 32768, "hello_time": 2, "forward_delay": 15, "max_age": 20},
    "igmp": {"snooping": true, "querier": false}
  },
  "vlans": [{"id": 1, "name": "default"}, {"id": 10, "name": "plc"}],
  "ports": {
    "lan1": {"enabled": true, "description": "uplink", "speed": "auto", "autoneg": true,
             "pvid": 1, "tagged": [10], "learning": true, "isolated": false,
             "stp_cost": 0, "stp_priority": 32},
    "lan2": {"enabled": true, "description": "PLC", "speed": "100full", "autoneg": true,
             "pvid": 10, "tagged": [], "learning": true, "isolated": true,
             "stp_cost": 0, "stp_priority": 32}
  },
  "static_fdb": [{"mac": "02:00:00:00:00:01", "port": "lan2", "vlan": 10}],
  "mirror": {"dest": "lan4", "sources": [{"port": "lan1", "direction": "both"}]}
}
```

- `speed`: `auto`, `1000full`, `100full`, `100half`, `10full`, `10half`. С `autoneg: true`
  выбранная скорость — единственная объявляемая; с `false` скорость задаётся жёстко
  (для 1000 нельзя).
- Access-порт: `pvid` без `tagged`. Trunk: `pvid` (native) + `tagged`. `pvid: 0` — только тегированный.
- В режиме `vlan_aware: false` VLAN-настройки портов хранятся, но не применяются;
  `static_fdb[].vlan` тогда должен быть 0.
- `stp.priority` кратен 4096; таймеры по 802.1D: `2*(forward_delay-1) >= max_age >= 2*(hello_time+1)`.
- `mirror: null` — зеркалирования нет.
- `network: null` — адресами управляет netplan. Пример режима роутера:

```json
"network": {
  "mode": "router",
  "wan": {"interface": "enx00e04c680026", "proto": "dhcp", "address": "", "gateway": "", "dns": []},
  "lan": {"proto": "dhcp", "address": "192.168.1.1/24", "gateway": "", "dns": [],
          "dhcp_server": {"enabled": true, "pool_start": "192.168.1.100", "pool_end": "192.168.1.199", "lease_time": 43200,
                          "dns": [], "static_leases": [{"mac": "02:00:00:00:00:10", "address": "192.168.1.10", "name": "plc"}]}},
  "firewall": {"enabled": true, "wan_allow": ["ssh", "web"],
               "port_forwards": [{"name": "cam", "proto": "tcp", "wan_port": 8554, "dest": "192.168.1.20", "dest_port": 554}]}
}
```

  `wan.proto: static` — нужны `address` (CIDR) и обычно `gateway`, `dns`. `lan.proto` действует
  только в режиме коммутатора. `dhcp_server.dns` пусто — клиентам отдаются DNS от WAN.
  `dest_port: 0` — тот же порт, что `wan_port`; `proto`: `tcp`, `udp`, `both`.

### Примеры

```sh
T=$(ssh root@192.168.30.82 cat /etc/napi-switch/token); U=http://192.168.30.82:8080/api/v1
# VLAN 10 для ПЛК на lan2, транк на lan5; откат через 60 с, если не подтвердить
curl -H "Authorization: Bearer $T" -X PATCH "$U/config?confirm=60&save=1" -d '{
  "bridge": {"vlan_aware": true},
  "vlans": [{"id": 1, "name": "default"}, {"id": 10, "name": "plc"}],
  "ports": {"lan2": {"pvid": 10}, "lan5": {"tagged": [10]}}}'
curl -H "Authorization: Bearer $T" -X POST "$U/config/confirm"
```

## Разработка

```
~/.local/go/bin/go test ./...      # логика плана, валидация, drop-in, merge patch
```

Код: `model.go` — конфигурация и валидация; `state.go` — чтение состояния ядра;
`plan.go` — расчёт команд (чистая функция, покрыта тестами); `network.go` — WAN/LAN/роутер:
модель, проверка, файлы networkd и правила nftables; `manager.go` — применение,
подтверждение, откат, сохранение; `persist.go` — drop-in файлы networkd; `api.go` — HTTP.
