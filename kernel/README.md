# Ядро с MRP (IEC 62439-2)

Ядро Armbian `current` (rockchip64, 6.18.55) с изменениями для кольцевого резервирования MRP.
Мост в ядре встроенный (`CONFIG_BRIDGE=y`), поэтому MRP включается только пересборкой.

| Файл | Что делает |
|---|---|
| `napi-mrp.config` | фрагмент конфига: `CONFIG_BRIDGE_MRP=y` (поверх `/boot/config-*` платы) |
| `ksz9477-mrp-sw-backup.patch` | драйвер ksz: принимает программный MRP (`sw_backup`) — пишет в статическую таблицу KSZ записи `01:15:4e:00:00:01..03` → только CPU, с битом OVERRIDE, чтобы кадры MRP доходили до CPU и с порта, который MRP заблокировал |
| `br_mrp-sw-fallback.patch` | мост: `br_mrp_add()` не падает на `-EOPNOTSUPP` от свитча. С патчем ksz не обязателен, оставлен для свитчей без поддержки MRP |

Патч ksz меняет `struct ksz_device` — ставить все модули `drivers/net/dsa/microchip/` вместе
(один `ksz_switch.ko` со старым `ksz_spi.ko` роняет ядро при загрузке).

Сборка (кросс, ~1 ч на 16 ядрах):

```
cp /boot/config-<плата> .config
scripts/config --set-str LOCALVERSION -napi-mrp --disable LOCALVERSION_AUTO --enable BRIDGE_MRP
make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- olddefconfig
make -j16 ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- Image modules
```

На плате ставится рядом со штатным ядром (`vmlinuz-/uInitrd-6.18.55-napi-mrp`), первая загрузка —
`oneshot_kernel=` в `armbianEnv.txt` + флаг ONCE (см. `boot.cmd`). Хук Armbian
`update-initramfs` перекидывает `/boot/uInitrd` на новое ядро — вернуть ссылку.

Проверено 2026-10-19 на плате №1 (MRM, кольцо lan1↔lan5 одним патч-кордом): кольцо
замыкается → вторичный порт в blocking за ~30 мс после линка, тестовые кадры приходят на
заблокированный порт, шторма нет; размыкание/замыкание 2 раза — без шторма.

2026-10-19 (вечер): ловушка кадров MRP теперь по номеру кольца, а не по порту — мост шлёт
роль кольца на все порты, и удаление одного кольца снимало ловушку у всех (найдено на двух
кольцах). Проверено: кольцо 1 остаётся CHK_RC и получает MRP_Test после добавления и
удаления кольца 2.
