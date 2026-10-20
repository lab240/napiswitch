# MRP ring test bed

A NAPI-Switch (MRM) and an Ubuntu VM or PC (MRC) in one MRP ring:

```
NAPI-Switch lanX ── host port A ─┐
                                 VM: mrpbr0 (MRC, mrp_server)
NAPI-Switch lanY ── host port B ─┘
```

Ubuntu's generic kernel has `CONFIG_BRIDGE_MRP=y` (Alpine and Debian kernels do not).
`napi-mrp` is in repo.napilab.ru for arm64 and amd64.

## VM

- Ubuntu Server 24.04+ (minimal is enough), three NICs: management (NAT or office LAN)
  and two ring ports.
- Ring ports: preferably USB Ethernet adapters passed through to the VM. A virtio/bridged
  NIC stays up when the host cable is pulled: this node cannot report the break, the MRM
  sees it only by lost test frames.
- Ring ports must have no IP and no netplan entry.

```
sudo ./mrp-vm.sh install
sudo ./mrp-vm.sh up enx1 enx2 --ip 192.168.1.50/24   # MRC, ring 1, profile 200
sudo ./mrp-vm.sh status
sudo ./mrp-vm.sh down
```

## Switch

Ring 1 as MRM on lanX/lanY with the same profile (napi-switchd API or web UI).
Close the ring cables only when both sides are up.

## Tests

- Ring closed: switch secondary blocked, `ring_state` closed on both sides, no storm.
- Traffic through the ring: `iperf3` and `ping -i 0.01` between a host on the switch and the VM.
- Pull each cable (switch side, host side) and plug it back: count lost pings, recovery time.
- Profile 200 vs 500. Reboot the switch with the ring closed.
