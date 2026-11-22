#!/usr/bin/env python3
"""Install local MCP dependencies and write machine-local default commands."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import venv

HEXSTRIKE_REV = "d689933ff579d839c676c82b231f8e98326c5f04"


def run(*args):
    subprocess.run([str(a) for a in args], check=True)


def environment(path, requirements):
    python = path / ("Scripts/python.exe" if os.name == "nt" else "bin/python3")
    if not python.exists():
        venv.EnvBuilder(with_pip=True).create(path)
    run(python, "-m", "pip", "install", "-r", requirements)
    return python


def entry(name, python, args):
    return {"name": name, "transport": "stdio", "command": str(python),
            "args": [str(a) for a in args], "env": {}, "enabled": True,
            "agents": ["mainagent", "planner", "worker", "retester"]}


def setup(root, hexstrike):
    state = root / "integrations"
    state.mkdir(parents=True, exist_ok=True)
    manifest = state / "mcp-defaults.json"
    entries = json.loads(manifest.read_text()) if manifest.exists() else []
    cve = root / "cve-intel-mcp"
    existing = cve / ".env/bin/python3"
    python = existing if existing.exists() else environment(state / "cve-venv", cve / "requirements.txt")
    entries = [e for e in entries if e["name"] != "cveintel"]
    entries.append(entry("cveintel", python, [cve / "cve_intel_mcp.py"]))
    if hexstrike:
        if os.name == "nt":
            raise RuntimeError("HexStrike autostart requires Linux/WSL or macOS; run this installer inside WSL")
        local = Path(os.getenv("ARTEX_HEXSTRIKE_DIR", str(Path.home() / "hexstrike-ai"))).expanduser().resolve()
        if not (local / "hexstrike_mcp.py").is_file():
            local = state / "hexstrike-ai"
            if not local.exists():
                run("git", "clone", "https://github.com/0x4m4/hexstrike-ai.git", local)
                run("git", "-C", local, "checkout", "--detach", HEXSTRIKE_REV)
            elif not (local / "hexstrike_mcp.py").is_file():
                raise RuntimeError(f"Refusing to overwrite incomplete HexStrike directory: {local}")
        existing = local / "hexstrike-env/bin/python3"
        python = existing if existing.exists() else environment(state / "hexstrike-venv", local / "requirements.txt")
        run(python, "-c", "import flask, requests, psutil; from mcp.server.fastmcp import FastMCP")
        entries = [e for e in entries if e["name"] != "hexstrike"]
        entries.append(entry("hexstrike", python, [root / "scripts/hexstrike-mcp.py", "--directory", local]))
    temporary = manifest.with_suffix(".tmp")
    temporary.write_text(json.dumps(entries, indent=2) + "\n")
    temporary.replace(manifest)
    print(f"MCP defaults ready: {manifest}. Existing ARTEX entries are preserved.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--hexstrike", choices=["yes", "no", "ask"], default="ask")
    args = parser.parse_args()
    choice = args.hexstrike
    if choice == "ask":
        choice = "yes" if sys.stdin.isatty() and input("Install/configure HexStrike MCP (Python dependencies; external scanners installed separately)? [y/N] ").strip().lower() in {"y", "yes"} else "no"
    setup(Path(__file__).resolve().parent.parent, choice == "yes")


if __name__ == "__main__":
    main()
