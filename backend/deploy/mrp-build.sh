#!/bin/bash
# Build and install Microchip's MRP daemon (mrp_server + mrp client) on the
# board. Runs ON the board (install.sh copies and starts it). Idempotent:
# skips the build when the pinned version is already installed.
set -e
CFM=7a50f4adcccf2d2e910816ea6d78446fea8fe7d8
MRP=177717ee893d5ccc6e31147b1b8a720c9b798b81
stamp=/usr/local/share/napi-switch/mrp.version
if [ "$(cat $stamp 2>/dev/null)" = "$MRP+napi2" ] && [ -x /usr/local/sbin/mrp_server ]; then
	echo "mrp: $MRP already installed"; exit 0
fi
export DEBIAN_FRONTEND=noninteractive
apt-get -qq install -y libnl-3-dev libnl-genl-3-dev libnl-route-3-dev libev-dev libmnl-dev cmake git pkg-config >/dev/null
src=/usr/src/napi-mrp
mkdir -p $src && cd $src
for r in cfm mrp; do
	[ -d $r ] || git clone -q https://github.com/microchip-ung/$r.git
done
git -C cfm fetch -q && git -C cfm checkout -q $CFM
git -C mrp fetch -q && git -C mrp checkout -q -f $MRP
# our fixes on top of the pinned commit (copied next to this script by install.sh)
for p in "$(dirname "$(realpath "$0")")"/mrp-patches/*.patch; do [ -e "$p" ] && git -C mrp apply "$p"; done
(mkdir -p cfm/build && cd cfm/build && cmake .. >/dev/null && make -s -j4)
(mkdir -p mrp/build && cd mrp/build && cmake -DLibCFM_INCLUDE_DIR=$src/cfm -DLibCFM_LIBRARY=$src/cfm/build/libcfm_netlink.a .. >/dev/null && make -s -j4)
install -m 755 mrp/build/mrp_server mrp/build/mrp /usr/local/sbin/
mkdir -p "$(dirname $stamp)" && echo $MRP+napi2 > $stamp
echo "mrp: installed $MRP"
