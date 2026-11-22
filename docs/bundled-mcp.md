# Bundled MCP servers

`install.sh`, `build.sh` and `build-cloud.sh` prepare **cveintel** automatically and ask whether to install **hexstrike**. Python 3 with venv/pip is required. HexStrike installation also requires Git. No sudo is used.

```bash
# Explicit opt-in for unattended builds/installations:
ARTEX_INSTALL_HEXSTRIKE=yes ./build.sh
# Configure an existing checkout without rebuilding the UI:
ARTEX_INSTALL_HEXSTRIKE=yes bash scripts/setup-mcp.sh
# Reuse a different HexStrike checkout:
ARTEX_HEXSTRIKE_DIR=/path/to/hexstrike-ai ARTEX_INSTALL_HEXSTRIKE=yes bash scripts/setup-mcp.sh
```

Without a terminal, the default answer is **no**. `ARTEX_INSTALL_HEXSTRIKE=no` skips HexStrike installation and preserves an earlier installation. `ARTEX_SETUP_MCP=0` skips dependency setup entirely (for an offline/CI build). A failed installation stops the build rather than reporting a working integration.

Python environments and machine-local commands live in ignored `integrations/`. An existing `~/hexstrike-ai` and its `hexstrike-env` are reused; otherwise the official [HexStrike repository](https://github.com/0x4m4/hexstrike-ai) is fetched at commit `d689933ff579d839c676c82b231f8e98326c5f04`. Existing checkouts are never reset or upgraded. Python requirements are those supplied by each server; they are version ranges, not a fully locked dependency set.

At startup ARTEX reads `integrations/mcp-defaults.json` beside the skills directory, adds missing servers, and grants new defaults to Main, Planner, Worker and Retester. Existing servers, environment variables, disabled states and visibility rules are preserved. Override the manifest path with `ARTEX_MCP_DEFAULTS`. Enabled cveintel/hexstrike servers are started and their tool cache refreshed on every ARTEX start; stdio clients also start on demand for agent sessions. Disabling a server in the UI prevents startup.

HexStrike's bundled adapter exposes **36 core tools**. Its API backend starts on demand on **127.0.0.1:8888**; startup is serialized with a file lock. It survives short-lived MCP clients and is reused across agents. Runtime logs are in `integrations/hexstrike-runtime/backend.log`. This adapter currently supports Linux/WSL and macOS, not native Windows. External executables such as nmap, nuclei and sqlmap are **not installed** by Python dependency setup; availability is reported by HexStrike `/health`.

CVEIntel needs no credentials. Optional `NVD_API_KEY` and `GITHUB_TOKEN` can be set in the MCP environment in ARTEX. Credentials are never copied into bundled defaults or release archives.

Release archives include `scripts/setup-mcp.sh`, the adapters and CVEIntel sources. Run the setup script on the destination machine before starting ARTEX. Host virtualenvs and machine-local manifests are not packaged.

Docker images include CVEIntel. Build with `--build-arg INSTALL_HEXSTRIKE=yes` to include HexStrike, or choose yes in the Docker installer. Compose persists dependencies in `mcp-integrations`; no HexStrike API port is published. This requires an image built from the updated Dockerfile; older published images do not contain the setup scripts.
