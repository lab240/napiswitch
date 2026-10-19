# Дополнение для сборщика образа NAPI-Switch: udev / systemd .link

Положить в образ (bsp-cli: `config/optional/boards/napi-switch/_packages/bsp-cli/etc/systemd/network/`)
шесть файлов из `backend/deploy/network/` (github.com/lab240/napiswitch, коммит df8802f):

- `10-trunk0.link` — GMAC (conduit DSA) называется `trunk0`, без MAC-altname;
- `10-napi-switchd-lan1.link` … `10-napi-switchd-lan5.link` — **802.3x flow control выключен**
  на портах свитча с первого согласования линка (как у Moxa EDS: «FDX Flow Ctrl» — Disabled).

Почему так:
- udev применяет `.link` в момент появления порта — раньше networkd и раньше подъёма линка;
- к устройству применяется только **первый** подходящий `.link`, поэтому файлы повторяют
  политики `99-default.link` (NamePolicy, AlternativeNamesPolicy, MACAddressPolicy);
- выключение принудительное (`AutoNegotiationFlowControl=no`, `Rx/TxFlowControl=no`):
  phylink при загрузке объявляет pause, хотя настройка читается как «выкл.», и udev пропустил бы
  «такую же» настройку;
- napi-switchd потом сам переписывает `10-napi-switchd-*.link` из своего конфига
  (`ports.X.flow_control`) — в образе они задают только значение по умолчанию.

Проверено на плате №1 20.10.2026 перезагрузкой: udev применил эти файлы (`ID_NET_LINK_FILE`),
все порты «flow control off» с первого линка, napi-switchd ни одной команды ethtool не выполнял.

Пример (`10-napi-switchd-lan1.link`):
```
# NAPI-Switch default (image): 802.3x flow control off on this port. napi-switchd
# regenerates this file from /etc/napi-switch/config.json (ports.X.flow_control).

[Match]
OriginalName=lan1
Driver=dsa

[Link]
NamePolicy=keep kernel database onboard slot path
AlternativeNamesPolicy=database onboard slot path mac
MACAddressPolicy=persistent
AutoNegotiationFlowControl=no
RxFlowControl=no
TxFlowControl=no
```
