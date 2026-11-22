import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).parent / file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


setup = load("setup_mcp", "setup-mcp.py")
adapter = load("hexstrike_adapter", "hexstrike-mcp.py")


class SetupTests(unittest.TestCase):
    def test_decline_and_repeat_preserve_existing_hexstrike(self):
        with tempfile.TemporaryDirectory(prefix="artex mcp ") as temp:
            root = Path(temp)
            state = root / "integrations"
            state.mkdir()
            manifest = state / "mcp-defaults.json"
            existing = {"name": "hexstrike", "command": "/custom/python", "enabled": False}
            manifest.write_text(json.dumps([existing]))
            with patch.object(setup, "environment", return_value=root / "venv/bin/python3") as install:
                setup.setup(root, False)
                setup.setup(root, False)
            entries = json.loads(manifest.read_text())
            self.assertEqual(len(entries), 2)
            self.assertEqual(next(e for e in entries if e["name"] == "hexstrike"), existing)
            self.assertEqual(install.call_count, 2)

    def test_failure_does_not_publish_partial_manifest(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            with patch.object(setup, "environment", side_effect=RuntimeError("dependency failure")):
                with self.assertRaisesRegex(RuntimeError, "dependency failure"):
                    setup.setup(root, True)
            self.assertFalse((root / "integrations/mcp-defaults.json").exists())

    def test_healthy_backend_is_reused(self):
        with tempfile.TemporaryDirectory() as temp:
            with patch.object(adapter, "healthy", return_value=True), patch.object(adapter.subprocess, "Popen") as spawn:
                url = adapter.ensure_backend(Path(temp), Path(temp), 8888)
                self.assertEqual(url, "http://127.0.0.1:8888")
                spawn.assert_not_called()

    def test_wrong_service_is_not_treated_as_hexstrike(self):
        import io
        with patch.object(adapter.urllib.request.OpenerDirector, "open", return_value=io.BytesIO(b'{"status":"healthy"}')):
            self.assertFalse(adapter.healthy("http://127.0.0.1:8888"))


if __name__ == "__main__":
    unittest.main()
