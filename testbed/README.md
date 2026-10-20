# MRP ring test bed

A NAPI-Switch (MRM) and a Linux PC in one ring. The PC is a plain bridge of two NICs
(MRP-unaware): it forwards the MRP frames, the switch's MRM blocks/unblocks its secondary
port and does the recovery.

```
NAPI-Switch lanX ── PC NIC 1 ─┐
                              PC: mrpbr0 (plain bridge)
NAPI-Switch lanY ── PC NIC 2 ─┘
```

The PC emulates the two MRC behaviours a plain bridge lacks (nftables table
`bridge napi_mrp` + a tcpdump listener, started by `up`):

- a ring port that lost link or was set down comes back blocked for data (MRP frames
  pass), and forwards on the next MRP_TopologyChange once its link is up, at most
  1 s after link-up. A plain bridge forwards at once. The MRM has already opened its
  secondary, because its test frames did not come back within 40 ms of its own
  link-up: a PC NIC reports link later. Result: a ~0.4 s loop.
- every MRP_TopologyChange flushes the FDB. Without that, after a break the MRM
  detects only by test frames (`ip link set down` on a USB NIC keeps the link up),
  the PC keeps sending through the port the MRM blocks again on reclosure, and the
  outage lasts 8–23 s until ARP/FDB entries age out.

Why not MRC on the PC: a stock kernel (Ubuntu has `CONFIG_BRIDGE_MRP=y`) refuses an MRP
ring on ordinary NICs with `EOPNOTSUPP`: bridge MRP needs a switchdev driver that takes
the ring (on the NAPI-Switch the ksz patch does it). An MRC needs a kernel with a software
fallback, or a second NAPI-Switch.

## PC

- Ubuntu Desktop Live USB is enough (real NICs, real link-down; nothing written to disk).
  Management over Wi-Fi or a third NIC: the NIC with the default route or the SSH session
  is refused as a ring port. NetworkManager is told to leave the ring ports alone.
- A VM works too, but a virtio NIC stays up when the host cable is pulled.

```
sudo ./mrp-vm.sh install                        # iproute2 iperf3 tcpdump ethtool nftables
sudo ./mrp-vm.sh up enp0s31f6 enx00e04c221648 --ip 10.9.9.2/24
sudo ./mrp-vm.sh status
sudo ./mrp-vm.sh down
```

## Order

1. Switch: ring 1 as MRM on lanX/lanY (napi-switchd API or web UI), profile 200.
2. PC: `up` (both ports join the bridge).
3. Plug the two cables: the ring closes through the PC, the MRM blocks its secondary.

## Tests

- Ring closed: `ring_state` closed on the switch, secondary blocked, no storm.
- Traffic through the ring: `iperf3` and `ping -i 0.01` between a host on the switch and the PC.
- Pull each cable (switch side, PC side) and plug it back: lost pings, recovery time.
- Profile 200 vs 500. Reboot the switch with the ring closed.
