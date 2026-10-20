#!/bin/bash
# MRP test node on an Ubuntu (24.04+) VM or PC: a Linux bridge with two ring
# ports and Microchip mrp_server as MRC (or MRM), for an MRP ring with a
# NAPI-Switch. Runtime only: nothing survives a reboot, `down` undoes it.
#
#   mrp-vm.sh install                 napilab repo + napi-mrp (mrp_server), iproute2, iperf3
#   mrp-vm.sh up IF1 IF2 [options]    bridge IF1+IF2, MRP ring, ports up last
#       --role mrc|mrm   (mrc)        --ring N (1)        --profile 200|500 (200)
#       --ip CIDR | --dhcp            address on the bridge for ping/iperf
#       --bridge NAME    (mrpbr0)
#   mrp-vm.sh status                  ring and port states
#   mrp-vm.sh down                    remove the ring and the bridge
#
# Ring ports must be free: no IP, not managed by netplan/NetworkManager
# (keep the VM's management NIC separate). Close the ring cable only after
# `up`: the NAPI-Switch side must be MRM with its secondary port blocked.
# In a VM the ring NICs should be passed-through USB adapters: a virtio NIC
# stays "up" when the host cable is pulled, so this node cannot report the
# break and the MRM sees it only by lost test frames.
set -e
MRP=${MRP_CLI:-/usr/sbin/mrp}
state=/run/napi-mrp-vm
die() { echo "mrp-vm: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root"

kernel_has_mrp() {
	local c=/boot/config-$(uname -r)
	if [ -r "$c" ]; then grep -q '^CONFIG_BRIDGE_MRP=y' "$c"; else zcat /proc/config.gz 2>/dev/null | grep -q '^CONFIG_BRIDGE_MRP=y'; fi
}

case $1 in
install)
	kernel_has_mrp || die "kernel $(uname -r) has no CONFIG_BRIDGE_MRP"
	arch=$(dpkg --print-architecture)
	if [ ! -f /usr/share/keyrings/napilab.gpg ]; then
		apt-get -qq update && apt-get -qq install -y curl gpg >/dev/null
		curl -fsSL https://repo.napilab.ru/napilab.gpg | gpg --dearmor >/usr/share/keyrings/napilab.gpg
	fi
	echo "deb [arch=$arch signed-by=/usr/share/keyrings/napilab.gpg] https://repo.napilab.ru stable main" \
		>/etc/apt/sources.list.d/napilab.list
	apt-get -qq update
	DEBIAN_FRONTEND=noninteractive apt-get -qq install -y napi-mrp iproute2 iperf3 tcpdump ethtool >/dev/null
	systemctl is-active -q napi-mrp || systemctl start napi-mrp
	echo "mrp-vm: napi-mrp $(dpkg-query -W -f '${Version}' napi-mrp) installed, mrp_server $(systemctl is-active napi-mrp)"
	;;
up)
	shift
	p=$1 s=$2
	[ -n "$p" ] && [ -n "$s" ] || die "usage: $0 up IF1 IF2 [--role mrc|mrm] [--ring N] [--profile 200|500] [--ip CIDR|--dhcp] [--bridge NAME]"
	shift 2
	role=mrc ring=1 prof=200 ip= dhcp= br=mrpbr0
	while [ $# -gt 0 ]; do
		case $1 in
		--role) role=$2; shift ;;
		--ring) ring=$2; shift ;;
		--profile) prof=$2; shift ;;
		--ip) ip=$2; shift ;;
		--dhcp) dhcp=1 ;;
		--bridge) br=$2; shift ;;
		*) die "unknown option $1" ;;
		esac
		shift
	done
	case $role in mrc|mrm) ;; *) die "--role mrc|mrm" ;; esac
	case $prof in 200|500) ;; *) die "--profile 200|500" ;; esac
	[ -e "$state" ] && die "already up ($(cat $state)), run down first"
	kernel_has_mrp || die "kernel $(uname -r) has no CONFIG_BRIDGE_MRP"
	for i in "$p" "$s"; do
		[ -d /sys/class/net/$i ] || die "no interface $i"
		[ -z "$(ip -o addr show dev "$i" scope global)" ] || die "$i has an IP address (management NIC?)"
		[ ! -e /sys/class/net/$i/master ] || die "$i is already in $(basename "$(readlink /sys/class/net/$i/master)")"
	done
	systemctl is-active -q napi-mrp || systemctl start napi-mrp
	sleep 1
	$MRP getmrp >/dev/null 2>&1 || die "mrp_server does not answer ($MRP getmrp)"

	# Ports stay down until the ring exists: no loop through this node meanwhile.
	ip link set dev "$p" down
	ip link set dev "$s" down
	ip link add name "$br" type bridge stp_state 0 vlan_filtering 0
	echo "$br $p $s $ring" >$state
	ip link set dev "$p" master "$br"
	ip link set dev "$s" master "$br"
	ip link set dev "$br" up
	$MRP addmrp bridge "$br" ring_nr "$ring" pport "$p" sport "$s" ring_role "$role" ring_recv "$prof"
	ip link set dev "$p" up
	ip link set dev "$s" up
	if [ -n "$ip" ]; then
		ip addr add "$ip" dev "$br"
	elif [ -n "$dhcp" ]; then
		if command -v dhcpcd >/dev/null; then dhcpcd -q "$br"
		elif command -v dhclient >/dev/null; then dhclient -1 "$br"
		else echo "mrp-vm: no dhcpcd/dhclient, use --ip" >&2; fi
	fi
	echo "mrp-vm: $br up, ring $ring $role pport $p sport $s profile $prof"
	"$0" status
	;;
status)
	[ -e "$state" ] || { echo "mrp-vm: down"; exit 0; }
	read -r br p s ring <$state
	$MRP getmrp || true
	bridge link show master "$br" || true
	ip -br addr show dev "$br"
	;;
down)
	[ -e "$state" ] || { echo "mrp-vm: down"; exit 0; }
	read -r br p s ring <$state
	# Secondary down first: never a loop while MRP goes away.
	ip link set dev "$s" down || true
	$MRP delmrp bridge "$br" ring_nr "$ring" || true
	ip link set dev "$p" nomaster 2>/dev/null || true
	ip link set dev "$s" nomaster 2>/dev/null || true
	ip link del dev "$br" || true
	rm -f $state
	echo "mrp-vm: $br removed ($p, $s left down)"
	;;
*)
	sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
