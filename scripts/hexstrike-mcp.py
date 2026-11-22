#!/usr/bin/env python3
"""Run HexStrike core MCP and start its loopback backend once per installation."""
import argparse
import contextlib
import fcntl
import importlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request

CORE_TOOLS = set("""nmap_scan rustscan_fast_scan masscan_high_speed autorecon_scan
subfinder_scan amass_scan httpx_probe katana_crawl arjun_scan paramspider_mining
gobuster_scan ffuf_scan dirsearch_scan feroxbuster_scan dirb_scan nuclei_scan
nikto_scan sqlmap_scan dalfox_xss_scan wpscan_analyze wafw00f_scan netexec_scan
enum4linux_scan smbmap_scan responder_credential_harvest hydra_attack john_crack
hashcat_crack trivy_scan prowler_scan kube_hunter_scan checksec_analyze
binwalk_analyze volatility_analyze ghidra_analysis execute_command""".split())


def healthy(url):
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(url + "/health", timeout=2) as response:
            value = json.load(response)
            return value.get("status") == "healthy" and "tools_status" in value
    except (OSError, ValueError, urllib.error.URLError):
        return False


def ensure_backend(directory, state, port):
    url = f"http://127.0.0.1:{port}"
    state.mkdir(parents=True, exist_ok=True)
    with (state / "backend.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if healthy(url):
            return url
        env = os.environ.copy()
        env["HEXSTRIKE_PORT"] = str(port)
        with (state / "backend.log").open("ab") as log:
            child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "--directory", str(directory),
                                      "--port", str(port), "--backend"],
                                     cwd=state, env=env, stdin=subprocess.DEVNULL,
                                     stdout=log, stderr=log, start_new_session=True)
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            if child.poll() is not None:
                raise RuntimeError(f"HexStrike backend exited; see {state / 'backend.log'}")
            if healthy(url):
                return url
            time.sleep(0.25)
        child.terminate()
        child.wait(timeout=5)
        raise RuntimeError(f"HexStrike backend startup timed out; see {state / 'backend.log'}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--port", type=int, default=8888)
    parser.add_argument("--backend", action="store_true")
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("port must be between 1 and 65535")
    sys.path.insert(0, str(args.directory.resolve()))
    if args.backend:
        backend = importlib.import_module("hexstrike_server")
        backend.app.run(host="127.0.0.1", port=args.port, debug=False, use_reloader=False)
        return
    state = Path(__file__).resolve().parent.parent / "integrations/hexstrike-runtime"
    url = ensure_backend(args.directory.resolve(), state, args.port)
    # Apply the bundled core selection without modifying the upstream checkout.
    with contextlib.redirect_stdout(sys.stderr):
        client = importlib.import_module("hexstrike_mcp")
        mcp = client.setup_mcp_server(client.HexStrikeClient(url, client.DEFAULT_REQUEST_TIMEOUT))
        for name in list(mcp._tool_manager._tools):
            if name not in CORE_TOOLS:
                mcp._tool_manager.remove_tool(name)
    mcp.run()


if __name__ == "__main__":
    main()
