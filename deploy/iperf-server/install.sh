#!/usr/bin/env bash
# Installs a persistent iperf3 server (systemd) on the WAN-side host.
# Run as root: sudo ./install.sh
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "Must run as root (sudo)." >&2
  exit 1
fi

if command -v apt-get >/dev/null 2>&1; then
  apt-get update -y
  apt-get install -y iperf3
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y iperf3
elif command -v yum >/dev/null 2>&1; then
  yum install -y iperf3
else
  echo "Could not detect a package manager — install iperf3 manually, then re-run this script." >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
install -m 0644 "${script_dir}/iperf3-server.service" /etc/systemd/system/iperf3-server.service
systemctl daemon-reload
systemctl enable --now iperf3-server

echo "iperf3 server is now running on port 5201/tcp+udp — check with: systemctl status iperf3-server"
