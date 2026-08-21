# iperf3-monitor

Continuous LAN-WAN throughput monitoring, using
[`iperf3_exporter`](https://github.com/sonh-nguyen/iperf3_exporter) + Prometheus +
Grafana + Alertmanager. The router is treated as a blackbox — nothing runs on it.

## Test topology

```
┌──────────────── LAN ────────────────┐                    ┌── iperf server (WAN) ──┐
│  docker-compose stack:               │                    │                        │
│   - gateway (nginx, single port 80) ─┤                    │                        │
│   - iperf3_exporter                  │   router (blackbox)│   iperf3 -s            │
│   - targets-admin  (IP management UI)┼── LAN → WAN ───────┼──                      │
│   - prometheus     (file_sd)         │                    │                        │
│   - grafana                          │                    │                        │
│   - alertmanager                     │                    │                        │
└───────────────────────────────────────┘                    └────────────────────────┘
```

`iperf3_exporter` (LAN) is always the iperf3 client. Two directions are probed against
each target, each its own Prometheus job:

- `iperf3-upload` (`reverse_mode=false`): LAN → WAN
- `iperf3-download` (`reverse_mode=true`): WAN → LAN

`targets-admin` manages the list of iperf servers being probed; Prometheus picks up
changes via `file_sd`, no restart needed.
