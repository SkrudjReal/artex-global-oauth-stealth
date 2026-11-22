#!/usr/bin/env bash
# ARTEX_INSTALL_HEXSTRIKE=yes|no|ask; noninteractive ask defaults to no.
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
if [ "${ARTEX_SETUP_MCP:-1}" = "0" ]; then
  exit 0
fi
command -v python3 >/dev/null 2>&1 || { printf 'Python 3 is required for bundled MCP servers\n' >&2; exit 1; }
exec python3 "$root/scripts/setup-mcp.py" --hexstrike "${ARTEX_INSTALL_HEXSTRIKE:-ask}"
