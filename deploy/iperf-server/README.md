# iperf server (WAN side)

The machine running `iperf3 -s` acts as the far-end probe point across the internet — it
has nothing to do with the Docker stack. It can be a VPS, a dedicated server, or any
machine with an address reachable from the LAN over the real WAN.

## Requirements

1. **Reachable from the LAN over the real WAN** — needs a public IP, or port-forward +
   DDNS. If this machine is also behind CGNAT (no public IP), it can't be used directly.
2. **Uplink faster than the link speed being measured** — otherwise this machine itself
   becomes the bottleneck, and the measurement will be wrong (it'll measure the WAN
   machine's bandwidth, not the LAN link's).
3. **Stable 24/7 uptime** — the exporter probes it continuously on the Prometheus cycle.

## Install

```bash
sudo ./install.sh
```

The script installs `iperf3`, installs `iperf3-server.service` (systemd, auto-restarts on
crash or reboot), and enables the service.

## Security

Default port is 5201/tcp+udp. Don't expose it publicly without restrictions — anyone
could abuse it as a free bandwidth-testing iperf server or as a reflector. Recommended:

- Restrict the firewall to only the public IP of the LAN being measured, e.g. with `ufw`:
  ```bash
  ufw allow from <YOUR_LAN_PUBLIC_IP> to any port 5201
  ```
- Or use a WireGuard/Tailscale tunnel between the LAN host running `iperf3_exporter` and
  this machine — more secure, and doesn't depend on whether your home IP is static.

## Verify

```bash
systemctl status iperf3-server
journalctl -u iperf3-server -f
```
