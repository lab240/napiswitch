#!/bin/bash
# Install napi-switchd on a NAPI-Switch board: ./install.sh root@192.168.30.82
# Builds the binary, copies it and the unit. Does NOT enable the service:
# run `systemctl enable --now napi-switchd` on the board when ready.
set -e
host=${1:?usage: $0 root@board}
cd "$(dirname "$0")/.."
GO=${GO:-$HOME/.local/go/bin/go}
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $GO build -trimpath -ldflags "-s -w" -o deploy/napi-switchd .
# One-shot KSZ register module (RMII clock fix), built against the board's kernel.
ssh "$host" 'rm -rf /usr/src/napi-kszreg && mkdir -p /usr/src/napi-kszreg /usr/lib/napi-switch'
scp -q kmod/kszreg/kszreg.c kmod/kszreg/Makefile "$host":/usr/src/napi-kszreg/
ssh "$host" 'cd /usr/src/napi-kszreg && make -s >/dev/null 2>&1 &&
	install -m 644 kszreg.ko /usr/lib/napi-switch/kszreg-$(uname -r).ko && echo "kszreg: built for $(uname -r)" ||
	echo "kszreg: build FAILED (linux-headers-$(uname -r) installed?) - RMII clock fix unavailable"'
scp -q deploy/napi-switchd "$host":/usr/local/sbin/napi-switchd.new
scp -q deploy/napi-switchd.service "$host":/etc/systemd/system/napi-switchd.service
ssh "$host" 'mv /usr/local/sbin/napi-switchd.new /usr/local/sbin/napi-switchd && systemctl daemon-reload &&
	if systemctl is-active -q napi-switchd; then systemctl restart napi-switchd; fi &&
	echo "installed: $(/usr/local/sbin/napi-switchd -h 2>&1 | head -1)"'
