import hashlib
import json
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from dependency_licenses import collect

spec = importlib.util.spec_from_file_location("release_check", Path(__file__).with_name("check-release.py"))
checks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checks)


class ReleaseChecks(unittest.TestCase):
    def test_strict_scan_inspects_unselected_and_hidden_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for filename in ("credentials.json", ".env", "private.pem"):
                (root / filename).write_text("sk-" + "synthetic" * 5)
            files, failures = checks.check(root, strict=True)
            self.assertEqual(len(files), 3)
            self.assertEqual(sum("provider-key" in item for item in failures), 3)
            self.assertEqual(sum("outside release allowlist" in item for item in failures), 3)

    def test_private_identifier_fingerprint_rejects_copied_fixture(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "config").mkdir()
            (root / "config/release-private-fingerprints.json").write_text(json.dumps([
                {"sha256": hashlib.sha256(b"fictional-customer").hexdigest()}]))
            (root / "README.md").write_text("https://git.example.test/Fictional-Customer/demo.git")
            _, failures = checks.check(root)
            self.assertEqual(len(failures), 1)
            self.assertIn("private identifier fingerprint=", failures[0])
            self.assertNotIn("fictional-customer", failures[0].lower())
            (root / "README.md").write_text("https://git.example.test/example-org/demo.git")
            self.assertEqual(checks.check(root)[1], [])

    @patch("dependency_licenses.subprocess.check_output", return_value='{"Main":true}')
    def test_npm_inventory_without_node_modules(self, _):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "ui").mkdir()
            (root / "ui/pnpm-lock.yaml").write_text(
                "lockfileVersion: '9.0'\npackages:\n\n  '@scope/example@1.2.3':\n"
                "    resolution: {}\n  ordinary@2.0.0:\n    resolution: {}\nsnapshots:\n")
            inventory, entries = collect(root)
            self.assertEqual([(p["name"], p["version"]) for p in inventory],
                             [("@scope/example", "1.2.3"), ("ordinary", "2.0.0")])
            self.assertTrue(all(p["license_text_status"] == "not_collected" for p in inventory))
            self.assertEqual(entries, [])


if __name__ == "__main__":
    unittest.main()
