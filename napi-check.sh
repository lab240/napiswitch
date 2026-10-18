#!/bin/bash
# NAPI-Switch: проверка канала RMII SoC<->KSZ9567 на любом экземпляре платы.
# Только чтение + 60 с flood ping. Запуск от root:
#   bash napi-check.sh <IP в сети lan-портов> [секунд=60] [размер=1000]
# В lan-порт должен быть воткнут кабель, у br0 должен быть IPv4.
DST=$1; DUR=${2:-60}; SIZE=${3:-1000}
[ -z "$DST" ] && { echo "usage: $0 <target_ip> [sec] [size]"; exit 1; }
[ "$(id -u)" = 0 ] || { echo "нужен root"; exit 1; }

# conduit = интерфейс, на котором висят lanX@...
CND=$(ip -o link | sed -n 's/^[0-9]*: lan1@\([^:]*\):.*/\1/p')
[ -z "$CND" ] && { echo "нет lan1@... - DSA не поднялся"; ip -br l; exit 1; }
g(){ ethtool -S "$CND" | awk -v k="$1" '$1==k":"{print $2+0}'; }

echo "=== $(hostname) $(date '+%F %T') ядро $(uname -r)"
echo "conduit: $CND"; ip -br l; ip -4 -br a
echo "uptime: $(uptime)"
for z in /sys/class/thermal/thermal_zone*/temp; do echo "temp $z: $(( $(cat $z)/1000 )) C"; done 2>/dev/null

echo "=== GRF (0x114 REF_CLK drive, 0x118 TX drive, 0x300 SOC_CON0/IO-voltage)"
python3 - <<'PY' 2>&1
import mmap,os,struct
f=os.open("/dev/mem",os.O_RDONLY|os.O_SYNC)
m=mmap.mmap(f,0x1000,mmap.MAP_SHARED,mmap.PROT_READ,offset=0xff000000)
for o in (0x114,0x118,0x300): print(" %#x = %#x"%(o,struct.unpack("<I",m[o:o+4])[0]))
PY
echo "(эталон платы #1: 0x114=0x300 0x118=0x50fc 0x300=0x190)"

echo "=== счётчики ошибок ДО"
ethtool -S "$CND" | grep -E 'p05_rx_(crc|align)_err|mmc_rx_(crc|length)_error'

echo "=== flood ping $DST, $DUR с, $SIZE байт"
printf "%-8s %5s %6s | %-12s | %-12s\n" time sent loss% "soc->sw err" "sw->soc crc"
end=$(( $(date +%s) + DUR )); T=0; L=0
while [ $(date +%s) -lt $end ]; do
  c1=$(( $(g s00_p05_rx_crc_err) + $(g s00_p05_rx_align_err) )); r1=$(g mmc_rx_crc_error)
  out=$(ping -q -f -I br0 -s $SIZE -w 1 $DST 2>&1)
  c2=$(( $(g s00_p05_rx_crc_err) + $(g s00_p05_rx_align_err) )); r2=$(g mmc_rx_crc_error)
  tx=$(echo "$out" | grep -oE "^[0-9]+ packets transmitted" | grep -oE "^[0-9]+")
  rx=$(echo "$out" | grep -oE "[0-9]+ received" | grep -oE "^[0-9]+")
  [ -z "$tx" ] || [ "$tx" = 0 ] && { echo "$(date +%T) нет трафика"; sleep 1; continue; }
  T=$((T+tx)); L=$((L+tx-rx))
  printf "%-8s %5d %5d%% | %-12d | %-12d\n" "$(date +%T)" $tx $((100*(tx-rx)/tx)) $((c2-c1)) $((r2-r1))
done

echo "=== счётчики ошибок ПОСЛЕ"
ethtool -S "$CND" | grep -E 'p05_rx_(crc|align)_err|mmc_rx_(crc|length)_error'
[ $T -gt 0 ] && echo "ИТОГ: отправлено $T, потеряно $L ($((100*L/T))%). Норма - 0%. Плата #1: ~35%."
