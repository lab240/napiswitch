# NAPI-Switch: что изменить в ядре и DTS относительно Armbian (для сборки образа)

База: дерево сборки Armbian (`qwa-armbian-build`, коммит 6a7d7953f), плата `napi-switch`,
`LINUXFAMILY=rockchip64`, `KERNEL_TARGET=current` (6.18.x, проверено на **6.18.55**).
Все патчи — относительно исходников **после** патчей Armbian (включая уже имеющийся
`net-dsa-microchip-enable-SGMII-port-7-on-KSZ9567.patch`). Файлы — в репозитории
github.com/lab240/napiswitch.

Проверено на плате №1 (19–20.10.2026), если не сказано иное.

## 1. Конфиг ядра

В `config/kernel/linux-rockchip64-current.config` (или фрагментом для платы):

| Опция | Зачем | Статус |
|---|---|---|
| `CONFIG_BRIDGE_MRP=y` | кольцевое резервирование MRP (IEC 62439-2). Мост в ядре встроенный (`CONFIG_BRIDGE=y`), модулем MRP не включить — только пересборка | **обязательно**, проверено |
| `CONFIG_SOFTLOCKUP_DETECTOR=y`, `CONFIG_BOOTPARAM_SOFTLOCKUP_PANIC=y` | зависание ядра на CPU → panic → перезагрузка | рекомендуется, **собрано, на плате ещё не загружалось** |
| `CONFIG_HARDLOCKUP_DETECTOR=y` (buddy), `CONFIG_BOOTPARAM_HARDLOCKUP_PANIC=y` | то же для жёсткой блокировки | рекомендуется, не проверено на плате |
| `CONFIG_DETECT_HUNG_TASK=y`, `CONFIG_DEFAULT_HUNG_TASK_TIMEOUT=120`, `CONFIG_BOOTPARAM_HUNG_TASK_PANIC=y` | задача висит в ядре > 120 с → panic → перезагрузка | рекомендуется, не проверено на плате |
| `CONFIG_PANIC_TIMEOUT=10` | перезагрузка через 10 с после panic (сейчас то же делает sysctl) | рекомендуется |

Фрагмент: `kernel/napi-mrp.config`. Причина детекторов: 19.10 ядро зависло на блокировке,
сеть и вход встали, а аппаратный сторож не сработал (systemd продолжал его «кормить»).
Требование: свитч либо работает, либо перезагружается.

## 2. Патчи ядра

Положить в `patch/kernel/archive/rockchip64-6.18/`:

**`net-dsa-microchip-ksz9477-software-MRP.patch`** — это `kernel/ksz9477-mrp-sw-backup.patch`.
**Обязательно для MRP.** Файлы: `drivers/net/dsa/microchip/ksz9477.c`, `ksz9477.h`,
`ksz_common.c`, `ksz_common.h`.

- У KSZ9567 нет аппаратного MRP; ядро запускает программный MRP, только если драйвер свитча
  принимает роль кольца с `sw_backup` (гарантирует доставку кадров MRP в CPU). Без патча
  `mrp addmrp` → `Operation not supported`.
- Патч пишет в статическую таблицу ALU KSZ записи `01:15:4e:00:00:01..03` → только порт CPU,
  с битом OVERRIDE: тестовые кадры MRP доходят до CPU и с порта, который MRP заблокировал.
- Ловушка учитывается по номеру кольца (до 8 колец): мост рассылает роль кольца на **все**
  порты моста.
- **Меняет `struct ksz_device`** — все модули `drivers/net/dsa/microchip/` должны быть
  собраны вместе (при сборке пакета ядра это так и есть; вручную один `ksz_switch.ko` со
  старым `ksz_spi.ko` роняет загрузку).
- Кандидат в upstream (netdev).

`br_mrp-sw-fallback.patch` — **не нужен** (с патчем ksz избыточен; оставлен для свитчей
без поддержки MRP).

## 3. DTS: `patch/kernel/archive/rockchip64-6.18/dt/rk3308-napi-switch.dts`

Патч `dts/rk3308-napi-switch-phy-irq.patch` (готовый файл целиком —
`dts/rk3308-napi-switch.dts`). В узел `&spi2 / switch@0` добавляется подузел `mdio` с
`ethernet-phy@0..4`, в `port@0..4` (lan1–lan5) — `phy-handle = <&swphyN>`.

- Зачем: без узла `mdio` внутренние PHY висят на общей шине и **опрашиваются раз в 1 с**;
  с ним драйвер регистрирует шину `SMI-0`, и смена линка приходит **прерыванием**
  (INTRP_N → GPIO4_B1, уже описано в DTS). Нужно для MRP: обрыв кольца замечается сразу.
- Проверено на плате оверлеем `dts/napi-ksz-phy-irq.dts` (то же самое) с 19.10: прерывания
  `SMI-0:00..04`, линки по прерыванию. После включения в DTS оверлей не нужен
  (на плате сейчас `user_overlays=napi-ksz-phy-irq` в armbianEnv.txt — убрать).
- DTS с патчем компилируется (dtc), в DTB 5 `phy-handle` и узел `mdio`.

Что в DTS **не меняется**:
- `&gmac`: `clock_in_out = "output"`, `phy-mode = "rmii"`, fixed-link 100 — правильно
  **для плат с перепаянным R29** (см. п. 4).
- `&usb20_otg` `dr_mode = "host"` + узел LAN7850 — верно; сама LAN7850 на плате №1 не
  определяется на USB (порт запитан, подключения нет) — проверяется железо, не DTS.

## 4. Железо, влияющее на образ

- **R29** (стрэп RXD6_1 KSZ9567): на прототипе на GND → порт CPU в RMII Clock Mode → CRC/потери
  до 50 %. Исправление: R29 → 10 кОм к 3.3VD (RMII Normal Mode, такт от SoC). Плата №1
  перепаяна 19.10: при загрузке 0x6301 = 0xC5, 0 потерь. Для неперепаянных плат napi-switchd
  ставит бит программно модулем `kszreg` — тогда в образе нужны `linux-headers` под ядро
  (или собранный `kszreg-<версия>.ko`).

## 5. Уровень образа (rootfs / bsp) — сейчас на плате, в образ стоит взять

| Что | Файл | Зачем |
|---|---|---|
| Имя conduit | `/etc/systemd/network/10-trunk0.link`: `[Match] Path=platform-ff4e0000.ethernet`, `[Link] Name=trunk0`, `AlternativeNamesPolicy=` | GMAC → `trunk0`, вне масок `e*`/`en*` netplan |
| Panic → reboot | `/etc/sysctl.d/91-napi-panic.conf`: `kernel.panic = 10`, `kernel.panic_on_oops = 1` | |
| Аппаратный сторож | `/etc/systemd/system.conf.d/napi-watchdog.conf`: `RuntimeWatchdogSec=30s`, `RebootWatchdogSec=2min` | dw_wdt |
| MRP-демон | Microchip `mrp` + `libcfm` (закреплены `MRP=177717ee`, `CFM=7a50f4ad`), служба `napi-mrp.service` | `backend/deploy/mrp-build.sh` |
| Управление | `napi-switchd` + `napi-switchd.service` (Type=notify, WatchdogSec=30) | `backend/deploy/install.sh` |

**Проверить при сборке:** в новом образе есть `mstpd` + `napi-rstp.service` (RSTP в
пространстве пользователя). napi-switchd пока управляет STP ядра (`stp_state 0/1`), а для MRP
выключает STP на мосту. Нужно согласовать: либо napi-rstp не включать вместе с napi-switchd,
либо научить napi-switchd работать с mstpd. На плате №1 (старый образ) mstpd нет — эта
связка не проверялась.
