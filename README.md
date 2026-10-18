# NAPI-Switch

Программное обеспечение управляемого промышленного коммутатора NAPI-Switch
(Rockchip RK3308 + Microchip KSZ9567, Linux DSA, Armbian/Debian).

- [`backend/`](backend/) — `napi-switchd`: REST API и встроенный веб-интерфейс управления.
  Порты, VLAN 802.1Q, STP, MAC-таблица, IGMP snooping, зеркалирование, счётчики;
  WAN/LAN и режим роутера (NAT, DHCP-сервер, межсетевой экран, проброс портов).
  Применение с подтверждением и автооткатом, сохранение через systemd-networkd.
- [`napi-check.sh`](napi-check.sh) — проверка канала RMII процессор↔свитч на экземпляре платы
  (только чтение и 60 с flood ping).

Сборка и установка — в [backend/README.md](backend/README.md).
