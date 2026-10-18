#!/bin/bash
# Install napi-switchd on a NAPI-Switch board: ./install.sh root@192.168.30.82
# Builds the binary, copies it and the unit. Does NOT enable the service:
# run `systemctl enable --now napi-switchd` on the board when ready.
set -e
host=${1:?usage: $0 root@board}
cd "$(dirname "$0")/.."
GO=${GO:-$HOME/.local/go/bin/go}
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $GO build -trimpath -ldflags "-s -w" -o deploy/napi-switchd .
scp -q deploy/napi-switchd "$host":/usr/local/sbin/napi-switchd.new
scp -q deploy/napi-switchd.service "$host":/etc/systemd/system/napi-switchd.service
ssh "$host" 'mv /usr/local/sbin/napi-switchd.new /usr/local/sbin/napi-switchd && systemctl daemon-reload &&
	if systemctl is-active -q napi-switchd; then systemctl restart napi-switchd; fi &&
	echo "installed: $(/usr/local/sbin/napi-switchd -h 2>&1 | head -1)"'
