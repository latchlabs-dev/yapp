#!/usr/bin/env python3
"""Scan the actual staged snapshot; never echo matched secret values."""
import importlib.util
from pathlib import Path
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("release_check", root / "scripts/check-release.py")
checks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checks)
with tempfile.TemporaryDirectory(prefix="piproxy-index-") as tmp:
    subprocess.run(["git", "checkout-index", "--all", f"--prefix={tmp}/"], cwd=root, check=True)
    files, failures = checks.check(Path(tmp), strict=True)
    for failure in failures:
        print(failure)
    print(f"Checked {len(files)} staged source files; {len(failures)} unresolved findings.")
    raise SystemExit(bool(failures))
