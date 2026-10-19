# systemd .link files for the image (`/etc/systemd/network/`)

udev applies a `.link` file as the interface appears, before networkd and before the link
comes up. Only the first matching `.link` file applies to a device, so ours repeat the
policies of `/usr/lib/systemd/network/99-default.link`.

| File | Does |
|---|---|
| `10-trunk0.link` | names the GMAC (DSA conduit, RMII to KSZ9567 port 6) `trunk0`, no MAC altname — outside netplan's `e*`/`en*` globs |
| `10-napi-switchd-lan1..5.link` | 802.3x flow control **off** on the switch ports from the first negotiation (`AutoNegotiationFlowControl=no`, `Rx/TxFlowControl=no`). Default like Moxa EDS ("FDX Flow Ctrl" factory default Disabled): with pause a slow port stalls every flow of the sender |

napi-switchd rewrites `10-napi-switchd-*.link` from its saved config (`ports.X.flow_control`)
and removes stale ones, so shipping them in the image only sets the default until the first
save. Off is forced (`AutoNegotiationFlowControl=no`) because phylink boots advertising pause
while reporting it off, and udev skips a setting equal to the current one.

Verified on board 1 (2026-10-20): after reboot `ID_NET_LINK_FILE` = these files, all ports
"flow control off" from the first link-up, napi-switchd issued no ethtool commands.
