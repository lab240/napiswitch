#!/bin/bash
# Build the NAPI-Switch packages (Debian trixie boards, Ubuntu 24.04+ test VMs):
#   napi-switchd_<ver>_arm64.deb  REST API + web UI + health watchdog
#   napi-mrp_<ver>_<arch>.deb     Microchip mrp_server + mrp client (MRP ring daemon),
#                                 arm64 for the board, amd64 for MRP test VMs
# Usage: deploy/build-deb.sh [outdir]   (default: deploy/deb; MRP_ARCHES="arm64 amd64")
# Needs: go (GOTOOLCHAIN=auto fetches the version in go.mod), gcc for each arch
# (native or <triplet>-gcc), cmake, git, curl, xz, dpkg-deb. No root: the
# libraries for mrp_server come from pinned Debian trixie packages unpacked into
# a private sysroot, so the binaries link against what the board has.
set -e
cd "$(dirname "$0")/.."
out=$(realpath -m "${1:-deploy/deb}")
work=${WORK:-$HOME/.cache/napi-switch-deb}
GO=${GO:-go}
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
mkdir -p "$out" "$work"

# MRP daemon sources, pinned (same as mrp-build.sh).
CFM=7a50f4adcccf2d2e910816ea6d78446fea8fe7d8
MRP=177717ee893d5ccc6e31147b1b8a720c9b798b81
# Debian trixie libraries mrp_server builds against.
DEBS="libev4t64 libev-dev libmnl0 libmnl-dev libnl-3-200 libnl-3-dev libnl-genl-3-200 libnl-genl-3-dev"
DEBIAN=https://deb.debian.org/debian

[ -z "$(git status --porcelain -- . 2>/dev/null)" ] || echo "WARNING: backend has uncommitted changes" >&2
rev=$(git rev-parse --short HEAD)
VER=0.1.0~git.$rev
MRPVER=0~git.${MRP:0:7}

MRP_ARCHES=${MRP_ARCHES:-arm64 amd64}

# GNU triplet and C compiler (native gcc when the host is that arch).
triplet() { case $1 in arm64) echo aarch64-linux-gnu ;; amd64) echo x86_64-linux-gnu ;; esac; }
ccfor() {
	local t
	t=$(triplet "$1")
	if [ "$(gcc -dumpmachine 2>/dev/null)" = "$t" ]; then echo gcc; else echo "$t-gcc"; fi
}

##############################
# napi-switchd
##############################
echo "napi-switchd $VER"
pkg=$work/napi-switchd
rm -rf "$pkg" && mkdir -p "$pkg/DEBIAN" "$pkg/usr/sbin" "$pkg/usr/lib/systemd/system"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $GO build -trimpath -ldflags "-s -w" -o "$pkg/usr/sbin/napi-switchd" .
sed 's|^ExecStart=/usr/local/sbin/napi-switchd|ExecStart=/usr/sbin/napi-switchd -mrp /usr/sbin/mrp|' \
	deploy/napi-switchd.service >"$pkg/usr/lib/systemd/system/napi-switchd.service"
grep -q '^ExecStart=/usr/sbin/napi-switchd' "$pkg/usr/lib/systemd/system/napi-switchd.service"
cat >"$pkg/DEBIAN/control" <<EOF
Package: napi-switchd
Version: $VER
Section: net
Priority: optional
Architecture: arm64
Depends: iproute2, ethtool, systemd, napi-mrp
Maintainer: NapiLab <info@napilab.ru>
Homepage: https://github.com/lab240/napiswitch
Description: NAPI-Switch management daemon (REST API and web UI)
 Configures the KSZ9567 switch of NAPI-Switch through the Linux bridge:
 ports, VLANs, STP, MRP rings, flow control. Web UI and REST API on :8080.
 Health watchdog reboots the board when the switch hangs.
EOF
cat >"$pkg/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
[ "$1" = configure ] || exit 0
[ -d /run/systemd/system ] && systemctl daemon-reload
if [ -z "$2" ]; then
	systemctl enable napi-switchd.service >/dev/null 2>&1 || true
	# napi-switchd drives kernel STP itself (and turns it off for MRP):
	# userspace RSTP (mstpd) on the same bridge would fight it.
	if systemctl is-enabled -q napi-rstp.service 2>/dev/null; then
		systemctl disable napi-rstp.service >/dev/null 2>&1 || true
		echo "napi-switchd: napi-rstp.service disabled (napi-switchd manages STP/MRP)"
	fi
	echo "napi-switchd: enabled, starts on next boot (or: systemctl start napi-switchd)"
elif [ -d /run/systemd/system ]; then
	systemctl try-restart napi-switchd.service || true
fi
EOF
cat >"$pkg/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ] && [ -d /run/systemd/system ]; then
	systemctl stop napi-switchd.service || true
fi
EOF
cat >"$pkg/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ] || [ "$1" = purge ]; then
	systemctl disable napi-switchd.service >/dev/null 2>&1 || true
	[ -d /run/systemd/system ] && systemctl daemon-reload || true
fi
if [ "$1" = purge ]; then
	rm -rf /var/lib/napi-switch
fi
EOF
chmod 755 "$pkg/DEBIAN/postinst" "$pkg/DEBIAN/prerm" "$pkg/DEBIAN/postrm"
dpkg-deb --root-owner-group --build "$pkg" "$out/napi-switchd_${VER}_arm64.deb" >/dev/null

##############################
# napi-mrp: sysroot from trixie
##############################
build_mrp() {
arch=$1
CC=$(ccfor "$arch")
command -v "$CC" >/dev/null || { echo "napi-mrp $arch: no $CC, skipped" >&2; return 0; }
echo "napi-mrp $MRPVER $arch"
sysroot=$work/sysroot-$arch
idx=$work/Packages-trixie-$arch
if [ ! -s "$idx" ] || [ -n "$(find "$idx" -mtime +7)" ]; then
	curl -fsSL "$DEBIAN/dists/trixie/main/binary-$arch/Packages.xz" | xz -d >"$idx.new"
	mv "$idx.new" "$idx"
fi
rm -rf "$sysroot" && mkdir -p "$sysroot" "$work/debs"
for p in $DEBS; do
	rec=$(awk -v p="$p" '$0=="Package: "p{f=1} f&&/^$/{exit} f' "$idx")
	file=$(echo "$rec" | sed -n 's/^Filename: //p')
	sum=$(echo "$rec" | sed -n 's/^SHA256: //p')
	[ -n "$file" ] || { echo "no $p in trixie" >&2; exit 1; }
	deb=$work/debs/$(basename "$file")
	if ! echo "$sum  $deb" | sha256sum -c --status 2>/dev/null; then
		curl -fsSL -o "$deb" "$DEBIAN/$file"
		echo "$sum  $deb" | sha256sum -c --status || { echo "sha256 mismatch: $deb" >&2; exit 1; }
	fi
	dpkg-deb -x "$deb" "$sysroot"
done
# -dev symlinks are absolute or relative to /usr/lib: make them resolve inside the sysroot.
find "$sysroot" -type l -lname '/*' | while read -r l; do ln -sfn "$sysroot$(readlink "$l")" "$l"; done

src=$work/src
mkdir -p "$src"
for r in cfm mrp; do
	[ -d "$src/$r" ] || git clone -q https://github.com/microchip-ung/$r.git "$src/$r"
done
git -C "$src/cfm" fetch -q && git -C "$src/cfm" checkout -q $CFM
git -C "$src/mrp" fetch -q && git -C "$src/mrp" checkout -q $MRP
lib=$sysroot/usr/lib/$(triplet "$arch")
cross=(-DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR="$(triplet "$arch" | cut -d- -f1)" -DCMAKE_C_COMPILER="$CC"
	-DCMAKE_BUILD_TYPE=Release
	-DLibNL_INCLUDE_DIR="$sysroot/usr/include/libnl3" -DLibNL_LIBRARY="$lib/libnl-3.so"
	-DLibNL_GENL_LIBRARY="$lib/libnl-genl-3.so"
	-DLibMNL_INCLUDE_DIR="$sysroot/usr/include" -DLibMNL_LIBRARY="$lib/libmnl.so"
	-DLibEV_INCLUDE_DIR="$sysroot/usr/include" -DLibEV_LIBRARY="$lib/libev.so"
	-DCMAKE_EXE_LINKER_FLAGS="-Wl,--as-needed -Wl,-rpath-link,$lib")
rm -rf "$src/cfm/build" "$src/mrp/build"
cmake -S "$src/cfm" -B "$src/cfm/build" "${cross[@]}" >/dev/null
make -s -C "$src/cfm/build" -j"$(nproc)" cfm_netlink
cmake -S "$src/mrp" -B "$src/mrp/build" "${cross[@]}" \
	-DLibCFM_INCLUDE_DIR="$src/cfm" -DLibCFM_LIBRARY="$src/cfm/build/libcfm_netlink.a" >/dev/null
make -s -C "$src/mrp/build" -j"$(nproc)" mrp mrp_server

pkg=$work/napi-mrp-$arch
rm -rf "$pkg" && mkdir -p "$pkg/DEBIAN" "$pkg/usr/sbin" "$pkg/usr/lib/systemd/system"
install -m 755 "$src/mrp/build/mrp_server" "$src/mrp/build/mrp" "$pkg/usr/sbin/"
"${CC%gcc}strip" "$pkg/usr/sbin/mrp_server" "$pkg/usr/sbin/mrp" 2>/dev/null || strip "$pkg/usr/sbin/"* 2>/dev/null || true
sed 's|^ExecStart=/usr/local/sbin/mrp_server|ExecStart=/usr/sbin/mrp_server|' \
	deploy/napi-mrp.service >"$pkg/usr/lib/systemd/system/napi-mrp.service"
grep -q '^ExecStart=/usr/sbin/mrp_server' "$pkg/usr/lib/systemd/system/napi-mrp.service"
# Runtime deps from what the binaries actually link (NEEDED), trixie package names.
needed=$(for b in "$pkg"/usr/sbin/*; do readelf -d "$b" | sed -n 's/.*Shared library: \[\(.*\)\]/\1/p'; done | sort -u)
glibc=$(for b in "$pkg"/usr/sbin/*; do objdump -T "$b" 2>/dev/null || "${CC%gcc}objdump" -T "$b"; done |
	grep -o 'GLIBC_[0-9.]*' | sed 's/GLIBC_//' | sort -uV | tail -1)
deps="libc6 (>= $glibc)"
for so in $needed; do
	case $so in
	libc.so.*|ld-linux*) ;;
	libev.so.4) deps="$deps, libev4t64" ;;
	libmnl.so.0) deps="$deps, libmnl0" ;;
	libnl-3.so.200) deps="$deps, libnl-3-200" ;;
	libnl-genl-3.so.200) deps="$deps, libnl-genl-3-200" ;;
	*) echo "unmapped library $so" >&2; exit 1 ;;
	esac
done
cat >"$pkg/DEBIAN/control" <<EOF
Package: napi-mrp
Version: $MRPVER
Section: net
Priority: optional
Architecture: $arch
Depends: $deps
Maintainer: NapiLab <info@napilab.ru>
Homepage: https://github.com/microchip-ung/mrp
Description: MRP (IEC 62439-2) ring daemon for NAPI-Switch
 Microchip mrp_server and mrp client, mrp $MRP with libcfm $CFM.
 Needs a kernel with CONFIG_BRIDGE_MRP and a switch driver that accepts
 software MRP (NAPI-Switch kernel). Used by napi-switchd.
EOF
cat >"$pkg/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
[ "$1" = configure ] || exit 0
systemctl enable napi-mrp.service >/dev/null 2>&1 || true
[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload
# Never restart a running mrp_server: it clears the kernel rings and leaves a
# closed ring without MRP or STP. An upgraded binary runs from the next boot.
if systemctl is-active -q napi-mrp.service; then
	echo "napi-mrp: mrp_server keeps running, the new version starts on next boot"
else
	systemctl start napi-mrp.service || true
fi
EOF
cat >"$pkg/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ] && [ -d /run/systemd/system ]; then
	systemctl stop napi-mrp.service || true
fi
EOF
cat >"$pkg/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ] || [ "$1" = purge ]; then
	systemctl disable napi-mrp.service >/dev/null 2>&1 || true
	[ -d /run/systemd/system ] && systemctl daemon-reload || true
fi
EOF
chmod 755 "$pkg/DEBIAN/postinst" "$pkg/DEBIAN/prerm" "$pkg/DEBIAN/postrm"
dpkg-deb --root-owner-group --build "$pkg" "$out/napi-mrp_${MRPVER}_${arch}.deb" >/dev/null
}
for a in $MRP_ARCHES; do build_mrp "$a"; done

ls -1 "$out"/napi-switchd_"${VER}"_arm64.deb "$out"/napi-mrp_"${MRPVER}"_*.deb
