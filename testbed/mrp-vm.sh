#!/bin/bash
# Ring node for an MRP test with a NAPI-Switch (MRM) on a Linux PC/VM/Live USB:
# a bridge of two ring ports. Runtime only: nothing survives a reboot, `down`
# undoes it.
#
# --role none (default): plain bridge, no MRP protocol here; the NAPI-Switch
# MRM does all the work. Works on any kernel. Two MRC behaviours are emulated
# (nftables bridge table napi_mrp + listener), without them the MRM sees
# loops and black holes that a real MRC ring never has:
#  - a ring port that lost link (or was set down) comes back BLOCKED: only
#    MRP frames pass, so the MRM sees the ring close and blocks its secondary
#    first; the port forwards on the next MRP_TopologyChange after its link is
#    up (or 1 s after link-up). A plain bridge forwards at once: the MRM opens
#    its secondary when the test frames do not come back within 40 ms of its
#    own link-up (a PC NIC reports link later), a loop of ~0.4 s.
#  - every MRP_TopologyChange flushes the FDB: else a break the MRM sees only by
#    lost test frames leaves this bridge sending through the port the MRM
#    blocks again on reclosure, until ARP/FDB entries age out (8-23 s measured).
# --role mrc|mrm: runs MRP here (Microchip mrp_server). A stock kernel refuses
# it on ordinary NICs (EOPNOTSUPP: bridge MRP needs a switchdev driver that
# takes the ring, like the NAPI-Switch ksz patch); needs a kernel with a
# software fallback (../kernel/br_mrp-sw-fallback.patch and more).
#
#   mrp-vm.sh install [--mrp]         iproute2 iperf3 tcpdump ethtool nftables; --mrp: + napilab repo, napi-mrp
#   mrp-vm.sh up IF1 IF2 [options]    bridge IF1+IF2 (+ MRP ring), ports up last
#       --role none|mrc|mrm (none)        --ring N (1)        --profile 200|500 (200)
#       --ip CIDR | --dhcp            address on the bridge for ping/iperf
#       --bridge NAME    (mrpbr0)
#   mrp-vm.sh status                  ring and port states
#   mrp-vm.sh down                    remove the ring and the bridge
#
# Ring ports: any IP on them is flushed, NetworkManager is told to leave
# them alone. Keep a separate management NIC (Wi-Fi is fine): the NIC with
# the default route or this SSH session is refused. Close the ring cable only after
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
	DEBIAN_FRONTEND=noninteractive apt-get -qq install -y iproute2 iperf3 tcpdump ethtool nftables >/dev/null
	if [ "$2" != --mrp ]; then
		echo "mrp-vm: tools installed (for --role mrc|mrm: $0 install --mrp)"
		exit 0
	fi
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
	role=none ring=1 prof=200 ip= dhcp= br=mrpbr0
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
	case $role in none|mrc|mrm) ;; *) die "--role none|mrc|mrm" ;; esac
	case $prof in 200|500) ;; *) die "--profile 200|500" ;; esac
	[ -e "$state" ] && die "already up ($(cat $state)), run down first"
	[ $role = none ] || kernel_has_mrp || die "kernel $(uname -r) has no CONFIG_BRIDGE_MRP"
	if [ $role = none ]; then
		for t in nft tcpdump; do command -v $t >/dev/null || die "no $t ($0 install)"; done
	fi
	# Never take the NIC we are managed through (default route, this SSH session).
	mgmt=$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p')
	sshdev=
	if [ -n "$SSH_CONNECTION" ]; then
		sship=$(echo "$SSH_CONNECTION" | awk '{print $3}')
		sshdev=$(ip -o addr show | awk -v a="$sship" '{split($4,x,"/")} x[1]==a {print $2}')
	fi
	for i in "$p" "$s"; do
		[ -d /sys/class/net/$i ] || die "no interface $i"
		[ "$i" != "$mgmt" ] || die "$i carries the default route (management NIC?)"
		[ "$i" != "$sshdev" ] || die "$i carries this SSH session"
		[ ! -e /sys/class/net/$i/master ] || die "$i is already in $(basename "$(readlink /sys/class/net/$i/master)")"
	done
	# NetworkManager (Ubuntu Desktop / Live) would run DHCP on the ring ports.
	nm=
	if command -v nmcli >/dev/null && systemctl is-active -q NetworkManager; then
		nm=1
		for i in "$p" "$s"; do nmcli device set "$i" managed no; done
	fi
	for i in "$p" "$s"; do ip addr flush dev "$i"; done
	if [ $role != none ]; then
		systemctl is-active -q napi-mrp || systemctl start napi-mrp
		sleep 1
		$MRP getmrp >/dev/null 2>&1 || die "mrp_server does not answer ($MRP getmrp)"
	fi

	# Ports stay down until the ring exists: no loop through this node meanwhile.
	ip link set dev "$p" down
	ip link set dev "$s" down
	ip link add name "$br" type bridge stp_state 0 vlan_filtering 0
	echo "$br $p $s $ring $role ${nm:--}" >$state
	[ -z "$nm" ] || nmcli device set "$br" managed no 2>/dev/null || true
	ip link set dev "$p" master "$br"
	ip link set dev "$s" master "$br"
	ip link set dev "$br" up
	if [ $role != none ] && ! $MRP addmrp bridge "$br" ring_nr "$ring" pport "$p" sport "$s" ring_role "$role" ring_recv "$prof"; then
		echo "mrp-vm: addmrp failed (stock kernel on ordinary NICs: use --role none)" >&2
		"$0" down
		exit 1
	fi
	if [ $role = none ]; then
		# Ports come up blocked (data), MRP frames pass.
		setsid "$0" _mrc-emu "$br" "$p" "$s" </dev/null >/dev/null 2>&1 &
		echo "$br $p $s $ring $role ${nm:--} $!" >$state
		for t in 1 2 3 4 5 6 7 8 9 10; do nft list set bridge napi_mrp blocked >/dev/null 2>&1 && break; sleep 0.2; done
		sleep 0.5
	fi
	ip link set dev "$p" up
	ip link set dev "$s" up
	if [ -n "$ip" ]; then
		ip addr add "$ip" dev "$br"
	elif [ -n "$dhcp" ]; then
		if command -v dhcpcd >/dev/null; then dhcpcd -q "$br"
		elif command -v dhclient >/dev/null; then dhclient -1 "$br"
		else echo "mrp-vm: no dhcpcd/dhclient, use --ip" >&2; fi
	fi
	if [ $role = none ]; then
		echo "mrp-vm: $br up, plain bridge $p + $s (MRC link-up blocking + FDB flush on MRP_TopologyChange)"
	else echo "mrp-vm: $br up, ring $ring $role pport $p sport $s profile $prof"; fi
	"$0" status
	;;
status)
	[ -e "$state" ] || { echo "mrp-vm: down"; exit 0; }
	read -r br p s ring role nm tcpid <$state
	echo "role $role${tcpid:+ (MRC emulation pid $tcpid)}"
	[ -z "$tcpid" ] || echo "blocked ports: $(nft list set bridge napi_mrp blocked 2>/dev/null | tr -d '\n\t' | sed -n 's/.*elements = { *\([^}]*\)}.*/\1/p')"
	[ "$role" = none ] || $MRP getmrp || true
	bridge link show master "$br" || true
	ip -br addr show dev "$br"
	;;
down)
	[ -e "$state" ] || { echo "mrp-vm: down"; exit 0; }
	read -r br p s ring role nm tcpid <$state
	[ -z "$tcpid" ] || kill -- -"$tcpid" 2>/dev/null || true
	nft delete table bridge napi_mrp 2>/dev/null || true
	rm -f /run/napi-mrp-vm.ports/*
	# Secondary down first: never a loop while MRP goes away.
	ip link set dev "$s" down || true
	[ "$role" = none ] || $MRP delmrp bridge "$br" ring_nr "$ring" 2>/dev/null || true
	ip link set dev "$p" nomaster 2>/dev/null || true
	ip link set dev "$s" nomaster 2>/dev/null || true
	ip link del dev "$br" || true
	rm -f $state
	if [ "$nm" = 1 ]; then
		for i in "$p" "$s"; do nmcli device set "$i" managed yes || true; done
		echo "mrp-vm: $br removed ($s down, ports back to NetworkManager)"
	else
		echo "mrp-vm: $br removed ($p, $s left down)"
	fi
	;;
_mrc-emu)
	# Runs in its own session (killed as a process group by `down`).
	br=$2 p=$3 s=$4
	mrp='ether daddr { 01:15:4e:00:00:01, 01:15:4e:00:00:02 } accept'
	nft delete table bridge napi_mrp 2>/dev/null || true
	nft -f - <<-EOF
	table bridge napi_mrp {
		set blocked { type ifname; elements = { "$p", "$s" }; }
		chain forward { type filter hook forward priority 0; policy accept;
			$mrp
			iifname @blocked counter drop
			oifname @blocked counter drop
		}
		chain input { type filter hook input priority 0; policy accept;
			$mrp
			iifname @blocked counter drop
		}
		chain output { type filter hook output priority 0; policy accept;
			oifname @blocked counter drop
		}
	}
	EOF
	# Per blocked port: "down" (no link) or the link-up time in ms.
	ports=/run/napi-mrp-vm.ports
	mkdir -p $ports
	echo down >$ports/$p
	echo down >$ports/$s
	unblock() {
		rm -f "$ports/$1"
		nft delete element bridge napi_mrp blocked "{ $1 }" 2>/dev/null || true
	}
	# MRP_TopologyChange: the MRM changed the ring. Flush the FDB; ports that
	# have link (>= 20 ms) forward. A port still without link stays blocked: the
	# MRM sends a TopologyChange also when it opens its secondary because this
	# port's link is not up yet; forwarding here right after would loop.
	tcpdump -l -n -i "$br" ether dst 01:15:4e:00:00:02 2>/dev/null | while read -r _; do
		ip link set dev "$br" type bridge fdb_flush || true
		now=$(date +%s%3N)
		for i in "$p" "$s"; do
			t=$(cat "$ports/$i" 2>/dev/null) || continue
			if [ "$t" != down ] && [ $((now - t)) -ge 20 ]; then unblock "$i"; fi
		done
	done &
	# Link lost or port set down: blocked until a TopologyChange, at most 1 s after link-up.
	ip monitor link | while read -r line; do
		for i in "$p" "$s"; do
			case $line in
			*" $i: <"*NO-CARRIER*|*" $i: <"*"state DOWN"*)
				echo down >"$ports/$i"
				nft add element bridge napi_mrp blocked "{ $i }" 2>/dev/null || true ;;
			*" $i: <"*LOWER_UP*)
				[ "$(cat "$ports/$i" 2>/dev/null)" = down ] || continue
				t=$(date +%s%3N)
				echo "$t" >"$ports/$i"
				(sleep 1; if [ "$(cat "$ports/$i" 2>/dev/null)" = "$t" ]; then unblock "$i"; fi) & ;;
			esac
		done
	done
	;;
*)
	sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
