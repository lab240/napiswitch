# napi-switchd REST API

REST API демона управления NAPI-Switch (RK3308 + Microchip KSZ9567). Через него
работает веб-интерфейс; то же самое можно делать из скриптов, SCADA, Ansible и т. п.

- Адрес: `http://<плата>:8080/api/v1` (порт задаётся флагом `-listen`).
- Формат: JSON в обе стороны, `Content-Type: application/json`.
- Корень `/` отдаёт веб-интерфейс (без авторизации, токен он спрашивает сам).

## Авторизация

Все запросы, кроме `GET /health`, требуют заголовок

```
Authorization: Bearer <token>
```

Токен лежит на плате в `/etc/napi-switch/token` (создаётся демоном при первом запуске, права 600).
Без токена или с неверным — `401 {"error": "missing or wrong bearer token"}`.

```sh
T=$(ssh root@192.168.30.82 cat /etc/napi-switch/token)
U=http://192.168.30.82:8080/api/v1
curl -s -H "Authorization: Bearer $T" $U/ports
```

## Как меняется конфигурация

Вся настройка — один документ конфигурации (см. «Модель конфигурации»). Менять его можно
целиком (`PUT`) или частично (`PATCH`, JSON merge patch). Демон:

1. проверяет документ целиком и возвращает **все** ошибки сразу (422);
2. сравнивает желаемое с тем, что реально стоит в ядре, и строит план — минимальный список
   команд (`ip`, `bridge`, `ethtool`, `tc`, `networkctl`, `nft`, запись файлов);
3. выполняет план; если команда упала — возвращает систему в прежнее состояние (409);
4. при `confirm=N` ждёт подтверждения N секунд, иначе **сам откатывает** изменение.
   Это защита от потери доступа: неудачная настройка VLAN или адресов откатится сама;
5. при `save=1` сохраняет конфигурацию на диск (после подтверждения, если оно ждётся).
   Сохранённая конфигурация применяется при загрузке.

Посмотреть план без применения — `POST /config/preview`.

Изменения сети (`network`) без `confirm > 0` отклоняются: ошибка в адресах почти всегда
означает потерю доступа к плате.

Пока одно изменение ждёт подтверждения, новые отклоняются (409) — сначала `confirm`
или `rollback`.

## Коды ответов

| Код | Когда | Тело |
|---|---|---|
| 200 | успех | результат |
| 400 | плохой JSON, неизвестное поле, неверный параметр запроса | `{error}` |
| 401 | нет токена или неверный | `{error}` |
| 404 | нет такого порта | `{error}` |
| 409 | изменение ждёт подтверждения; нечего подтверждать; команда плана упала | `{error, apply_error?: {command, error, restored}, commands?}` |
| 422 | конфигурация не прошла проверку | `{error, problems: [...], warnings: [...]}` |
| 500 | внутренняя ошибка (не удалось прочитать состояние ядра и т. п.) | `{error}` |

`warnings` — предупреждения, которые изменение не блокируют (например «DHCP-сервер будет
отвечать на lan5, где есть линк»).

Пример 422:

```json
{
  "error": "invalid config: port lan2: stp_priority 0..63",
  "problems": ["port lan2: stp_priority 0..63"],
  "warnings": []
}
```

---

## Состояние (только чтение)

### `GET /health`

Без авторизации. `{"status": "ok", "version": "0.1.0"}`

### `GET /system`

Идентификация и здоровье устройства.

```json
{
  "name": "napi-sw-1", "location": "цех 2", "contact": "",
  "hostname": "napi-switch", "model": "NAPI-Switch (Microchip KSZ9567)",
  "kernel": "6.12.x", "daemon_version": "0.1.0",
  "uptime_s": 86400, "temperatures_c": [48.2],
  "bridge": "br0", "bridge_mac": "f6:3e:43:2b:92:47",
  "addresses": {"br0": ["192.168.30.81/24"], "enx00e04c680026": ["192.168.30.82/24"]},
  "cpu_port": {"interface": "trunk0", "rx_crc_errors": 0, "switch_rx_crc_errors": 0, "switch_rx_align_errors": 0}
}
```

`cpu_port` — счётчики ошибок канала RMII между SoC и свитчем.

### `GET /capabilities`

Что поддерживает устройство — по этому строится интерфейс.

```json
{
  "switch": "KSZ9567",
  "ports": {"lan1": {"speeds": ["1000full", "100full", "100half", "10full", "10half"], "copper": true},
            "sfp": {"speeds": [], "copper": false}},
  "vlan": {"ids": "1-4094", "shared_learning_mod": 128, "qinq": false},
  "fdb": {"dynamic_max": 4096, "static": true},
  "stp": {"protocols": ["stp"], "rstp": false, "mstp": false},
  "igmp": {"snooping": true, "querier": true},
  "mirror": {"sessions": 1, "directions": ["rx", "tx", "both"]},
  "isolation": true, "learning": true, "jumbo": false, "lag": false, "rate_limit": false,
  "qos": {"queues": 4, "planned": true},
  "router": {"nat": true, "dhcp_server": true, "firewall": true, "port_forwarding": true}
}
```

### `GET /ports`, `GET /ports/{name}`

Список портов свитча или один порт. Значения — из ядра (что реально стоит), кроме
`pvid`/`tagged`, которые берутся из конфигурации.

```json
{
  "name": "lan5", "hw_port": "p4", "description": "uplink", "enabled": true,
  "mac": "12:df:33:92:d6:52",
  "link": {
    "up": true, "speed": 1000, "duplex": "full", "autoneg": true,
    "supported_modes": ["10baseT/Half", "10baseT/Full", "100baseT/Half", "100baseT/Full", "1000baseT/Full"],
    "advertised_modes": ["..."], "partner_modes": ["..."],
    "sqi": 7, "sqi_max": 7
  },
  "speed": "auto", "autoneg": true,
  "bridged": true, "pvid": 1, "tagged": [],
  "vlans_running": [{"id": 1, "pvid": true, "untagged": true}],
  "stp_state": "forwarding", "stp_cost": 5, "stp_priority": 32,
  "learning": true, "isolated": false
}
```

- `link.speed` — Мбит/с, 0 — нет линка или неизвестно.
- `sqi` / `sqi_max` — качество сигнала в кабеле (100/1000BASE-T), `-1` — недоступно.
- `bridged: false` — порт не в мосту (например, `sfp`): VLAN, STP, learning к нему не применяются.
- `stp_state`: `disabled`, `listening`, `learning`, `forwarding`, `blocking`.

### `GET /ports/{name}/counters`

```json
{
  "port": "lan5",
  "summary": {
    "rx_bytes": 0, "tx_bytes": 0,
    "rx_unicast": 0, "rx_multicast": 0, "rx_broadcast": 0,
    "tx_unicast": 0, "tx_multicast": 0, "tx_broadcast": 0,
    "rx_errors": 0, "tx_errors": 0, "collisions": 0,
    "rx_discards": 0, "tx_discards": 0, "rx_pause": 0, "tx_pause": 0
  },
  "mib": {"rx_hi": 0, "rx_undersize": 0, "...": 0}
}
```

`summary` — сводка; `mib` — все MIB-счётчики свитча как есть. `rx_errors` = CRC + align +
symbol + undersize + oversize + fragments + jabbers.

### `POST /ports/{name}/counters/clear`

Обнуляет счётчики порта. Аппаратные счётчики не сбрасываются: демон запоминает базу и
дальше показывает разницу. После перезапуска демона база теряется.
`{"status": "cleared"}`

### `GET /vlans`

```json
{
  "vlan_aware": true,
  "vlans": [
    {"id": 1, "name": "default", "mgmt": true,
     "members": [{"port": "lan1", "untagged": true, "pvid": true}]},
    {"id": 10, "name": "plc", "mgmt": false,
     "members": [{"port": "lan2", "untagged": true, "pvid": true}, {"port": "lan5", "untagged": false, "pvid": false}]}
  ]
}
```

VLAN — из конфигурации, участники — из ядра. `mgmt` — VLAN управления (через него доступен
адрес br0).

### `GET /fdb`

MAC-таблица. Фильтры: `?port=lan1`, `?vlan=10`, `?type=dynamic|static|local`.

```json
{
  "ageing_time": 300,
  "entries": [{"mac": "18:fd:74:68:44:dd", "port": "lan5", "vlan": 0, "type": "dynamic", "offloaded": false}]
}
```

`vlan: 0` — мост не в режиме 802.1Q. `local` — собственный адрес порта.

### `DELETE /fdb`

Очистить динамические записи. `?port=lan1` — только на этом порту, без параметра — на всех.
`{"flushed": ["lan1"]}`

### `GET /stp`

```json
{
  "enabled": true, "protocol": "stp",
  "bridge_id": "8000.f6:3e:43:2b:92:47", "root_id": "8000.18:fd:74:68:44:dd",
  "is_root": false, "root_port": "lan5", "root_path_cost": 5,
  "priority": 32768, "hello_time": 2, "forward_delay": 15, "max_age": 20,
  "topology_change": false,
  "ports": [{"port": "lan5", "state": "forwarding", "cost": 5, "priority": 32}]
}
```

### `GET /multicast`

`{"snooping": true, "querier": false, "mdb": [...]}` — `mdb` в формате `bridge -j -d mdb show`.

### `GET /mirror`

`{"running": {"dest": "lan4", "sources": [{"port": "lan1", "direction": "both"}]}, "sessions_max": 1}`
или `"running": null`.

### `GET /network`

Адреса и маршрутизация (L3).

```json
{
  "managed": false, "mode": "unmanaged",
  "interfaces": [
    {"name": "enx00e04c680026", "role": "", "state": "routable",
     "carrier": true, "speed": 1000, "duplex": "full",
     "mac": "00:e0:4c:68:00:26", "driver": "r8152",
     "network_file": "/run/systemd/network/10-netplan-usb-eth-interfaces.network",
     "addresses": ["192.168.30.82/24"], "dhcp": true,
     "gateway": "192.168.30.1", "dns": ["192.168.30.1", "8.8.8.8"]},
    {"name": "br0", "role": "lan", "...": "..."}
  ],
  "wan_candidates": ["enx00e04c680026"],
  "dhcp_leases": [{"mac": "02:00:00:00:00:10", "address": "192.168.1.100", "hostname": "plc", "expires": 1760800000}],
  "nft_available": true, "firewall_active": false, "web_port": 8080,
  "suggested": {"mode": "switch", "wan": {"...": "..."}, "lan": {"...": "..."}, "firewall": {"...": "..."}}
}
```

- `managed: false` — адресами управляет netplan; `mode`: `unmanaged`, `switch`, `router`.
- `interfaces` — WAN-кандидаты (физические сетевые карты платы вне свитча) и мост br0.
  `role`: `wan`, `lan` или пусто. `network_file` — какой файл networkd сейчас действует.
- `speed` у WAN — то, о чём договорилась карта. USB 2.0 реально передаёт меньше 1 Гбит/с.
- `dhcp_leases` — адреса, выданные DHCP-сервером роутера; `expires` — Unix-время.
- `suggested` — готовая секция `network`, повторяющая текущие настройки netplan. Её можно
  отправить как есть, чтобы взять управление адресами.

### `GET /events`

Журнал изменений (последние события в памяти):
`[{"time": "2026-10-18T17:08:32+03:00", "message": "saved config applied (0 commands)"}]`

---

## Конфигурация

### `GET /config`

```json
{"applied": {...}, "saved": {...}, "unsaved_changes": false,
 "pending": {"deadline": "2026-10-18T17:10:00+03:00", "save": true}}
```

`applied` — действующая, `saved` — на диске (применяется при загрузке), `pending` — `null`
или ожидающее подтверждения изменение.

### `PATCH /config?confirm=60&save=1`

JSON merge patch (RFC 7396) поверх действующей конфигурации: объекты сливаются, массивы
заменяются целиком, `null` удаляет ключ (`"mirror": null` — выключить зеркалирование).

### `PUT /config?confirm=60&save=1`

Полная замена конфигурации.

Параметры обоих:

| Параметр | Значение |
|---|---|
| `confirm` | 0..3600 с. 0 (по умолчанию) — без подтверждения. Для `network` обязательно > 0 |
| `save` | `1` / `true` — сохранить на диск (после подтверждения) |

Ответ:

```json
{
  "commands": [{"args": ["bridge", "vlan", "add", "vid", "10", "dev", "lan2", "pvid", "untagged"]}],
  "warnings": [],
  "pending": {"deadline": "2026-10-18T17:10:00+03:00", "save": true},
  "saved_files": ["/etc/napi-switch/config.json"]
}
```

### `POST /config/preview`

Проверка и план без применения. Тело — patch, как у `PATCH`; `?mode=replace` — полная
конфигурация, как у `PUT`. Ответ как у `PATCH` плюс `files` — содержимое файлов, которые
будут записаны (networkd, nft):

```json
{"commands": [...], "warnings": [...],
 "files": {"/run/systemd/network/03-napi-switchd-lan.network": "[Match]\nName=br0\n..."}}
```

### `POST /config/confirm`

Подтвердить ожидающее изменение (и сохранить, если было `save=1`). 409, если подтверждать
нечего.

### `POST /config/rollback`

Откатить ожидающее изменение сразу, не дожидаясь таймера. `{"status": "rolled back"}`

### `POST /config/save`

Сохранить действующую конфигурацию на диск. `{"saved_files": [...]}`

---

## Модель конфигурации

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
             "stp_cost": 0, "stp_priority": 32}
  },
  "static_fdb": [{"mac": "02:00:00:00:00:01", "port": "lan2", "vlan": 10}],
  "mirror": {"dest": "lan4", "sources": [{"port": "lan1", "direction": "both"}]},
  "network": null
}
```

### `system`

Имя, расположение, контакт — для идентификации (до 64 символов).

### `bridge`

| Поле | Значение |
|---|---|
| `vlan_aware` | режим 802.1Q. `false` — все порты в одном домене, VLAN-настройки портов хранятся, но не применяются |
| `mgmt_vlan` | VLAN, в котором доступен адрес br0 (только при `vlan_aware`) |
| `ageing_time` | время жизни динамических MAC, с (10..1000000) |
| `stp.enabled` | STP (802.1D). RSTP не поддерживается |
| `stp.priority` | приоритет моста, 0..61440, кратен 4096 |
| `stp.hello_time`, `forward_delay`, `max_age` | таймеры, с; должно выполняться `2*(forward_delay-1) >= max_age >= 2*(hello_time+1)` |
| `igmp.snooping`, `igmp.querier` | IGMP snooping и querier |

### `vlans`

`[{id: 1..4094, name}]`. VLAN, на который ссылаются порты, должен быть в списке.
Учтите: у KSZ9567 128 FID, VLAN с одинаковым `id mod 128` делят таблицу MAC.

### `ports.<name>`

| Поле | Значение |
|---|---|
| `enabled` | порт включён |
| `description` | имя порта (до 64 символов), видно в `ip link` как alias |
| `speed` | `auto`, `1000full`, `100full`, `100half`, `10full`, `10half` (из `capabilities.ports.<name>.speeds`) |
| `autoneg` | `true` — выбранная скорость объявляется как единственная; `false` — скорость задаётся жёстко (для 1000 нельзя). При `auto` всегда `true` |
| `pvid` | VLAN без тега (access / native), 0 — нет |
| `tagged` | VLAN с тегом (trunk) |
| `learning` | изучать MAC-адреса на порту |
| `isolated` | изолированные порты не обмениваются трафиком друг с другом, только с неизолированными |
| `stp_cost` | стоимость пути STP, 0 — автоматически по скорости |
| `stp_priority` | приоритет порта STP, 0..63 |

Access-порт: `pvid` без `tagged`. Trunk: `pvid` (native) + `tagged`. Только тегированный: `pvid: 0`.

### `static_fdb`

Статические записи MAC-таблицы: `{mac, port, vlan}`. При `vlan_aware: false` `vlan` должен быть 0.

### `mirror`

Зеркалирование (аппаратное, одна сессия): `dest` — порт-анализатор, `sources` —
`{port, direction: rx|tx|both}`. `null` — выключено. `dest` не может быть источником.

### `network`

`null` — адресами управляет netplan (как после установки). Если указана — адресами управляет
демон через systemd-networkd, межсетевым экраном — через nftables.

```json
"network": {
  "mode": "router",
  "wan": {"interface": "enx00e04c680026", "proto": "dhcp", "address": "", "gateway": "", "dns": []},
  "lan": {
    "proto": "dhcp", "address": "192.168.1.1/24", "gateway": "", "dns": [],
    "dhcp_server": {
      "enabled": true, "pool_start": "192.168.1.100", "pool_end": "192.168.1.199", "lease_time": 43200, "dns": [],
      "static_leases": [{"mac": "02:00:00:00:00:10", "address": "192.168.1.10", "name": "plc"}]
    }
  },
  "firewall": {
    "enabled": true, "wan_allow": ["ssh", "web"],
    "port_forwards": [{"name": "cam", "proto": "tcp", "wan_port": 8554, "dest": "192.168.1.20", "dest_port": 554}]
  }
}
```

| Поле | Значение |
|---|---|
| `mode` | `switch` — br0 обычный клиент сети; `router` — порты свитча это LAN за NAT, WAN — выход наружу |
| `wan.interface` | сетевая карта WAN (из `GET /network` → `wan_candidates`) |
| `wan.proto` | `dhcp` или `static` (тогда `address` в виде CIDR, `gateway`, `dns`) |
| `wan.dns` | DNS-серверы; при `dhcp` пусто — взять из DHCP |
| `lan.proto`, `lan.gateway`, `lan.dns` | адрес br0 в режиме `switch` (`dhcp` / `static`) |
| `lan.address` | в режиме `router` — адрес роутера в LAN (CIDR); в `switch` + `static` — адрес br0 |
| `dhcp_server.enabled` | DHCP-сервер в LAN (только `router`) |
| `pool_start`, `pool_end` | диапазон выдаваемых адресов включительно, например `192.168.1.100` – `192.168.1.199`. Должен лежать в подсети LAN и не содержать адрес роутера |
| `lease_time` | срок аренды, с |
| `dhcp_server.dns` | DNS для клиентов; пусто — те, что получены на WAN |
| `static_leases` | закреплённые адреса `{mac, address, name}` |
| `firewall.enabled` | с WAN проходят только ответы на соединения из LAN, проброшенные порты и `wan_allow`. `false` — NAT работает, вход с WAN не фильтруется |
| `wan_allow` | что разрешено с WAN к самой плате: `ssh` (22), `web` (порт веб-интерфейса), `ping` |
| `port_forwards` | проброс `wan_port` → `dest:dest_port`; `proto`: `tcp`, `udp`, `both`; `dest_port: 0` — тот же порт |

Проверки: адреса и шлюзы корректны и в своей подсети, LAN не пересекается с WAN, диапазон
DHCP внутри подсети, начало не позже конца, адрес роутера не входит, закреплённые адреса внутри подсети,
проброшенный порт не конфликтует с открытыми ssh/web. Предупреждения: DHCP-сервер на
портах, где есть линк (возможно, подключена чужая сеть); не установлен nftables; закрыт
доступ ssh/web с WAN.

Вернуть управление netplan на ходу (`network: null` после того, как сеть была под
управлением демона) нельзя — только удалив файлы `/etc/systemd/network/04-napi-switchd-*`
и перезагрузив плату.

---

## Примеры

```sh
T=$(ssh root@192.168.30.82 cat /etc/napi-switch/token); U=http://192.168.30.82:8080/api/v1
H="Authorization: Bearer $T"

# Имя и скорость порта
curl -H "$H" -X PATCH "$U/config?save=1" -d '{"ports": {"lan3": {"description": "камера", "speed": "100full"}}}'

# VLAN 10 для ПЛК на lan2, транк на lan5; откат через 60 с, если не подтвердить
curl -H "$H" -X PATCH "$U/config?confirm=60&save=1" -d '{
  "bridge": {"vlan_aware": true},
  "vlans": [{"id": 1, "name": "default"}, {"id": 10, "name": "plc"}],
  "ports": {"lan2": {"pvid": 10}, "lan5": {"tagged": [10]}}}'
curl -H "$H" -X POST "$U/config/confirm"

# Посмотреть, что будет сделано при переходе в режим роутера
S=$(curl -s -H "$H" $U/network | python3 -c 'import json,sys; n=json.load(sys.stdin)["suggested"]; n["mode"]="router"; print(json.dumps({"network": n}))')
curl -H "$H" -X POST "$U/config/preview" -d "$S"

# Зеркалировать lan1 на lan4, потом выключить
curl -H "$H" -X PATCH "$U/config" -d '{"mirror": {"dest": "lan4", "sources": [{"port": "lan1", "direction": "both"}]}}'
curl -H "$H" -X PATCH "$U/config" -d '{"mirror": null}'

# Ошибки CRC на порту
curl -s -H "$H" $U/ports/lan1/counters | python3 -c 'import json,sys; print(json.load(sys.stdin)["summary"]["rx_errors"])'
```
