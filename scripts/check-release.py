#!/usr/bin/env python3
"""Check candidate contents without printing matched credentials.

This supplements a full-history secret scanner; it is not a replacement for one.
Private operations may supply a deny-list file (one private literal per line).
"""
import argparse
import hashlib
import json
import re
from pathlib import Path

from release_files import source_files

PATTERNS = {
    "private-key": re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
    "provider-key": re.compile(rb"(?<![A-Za-z0-9])sk-(?:ant-)?[A-Za-z0-9_-]{24,}"),
    "proxy-key": re.compile(rb"cpk_live_[A-Za-z0-9_-]{20,}"),
    "aws-key": re.compile(rb"AKIA[0-9A-Z]{16}"),
    "google-key": re.compile(rb"AIza[0-9A-Za-z_-]{30,}"),
}
FORBIDDEN_ROOTS = {"reports", "evals", "deploy", ".pi", "secrets", ".private"}


def check(root, deny_list=None, strict=False):
    allow_file = root / "config/release-scan-allowlist.json"
    allow = json.loads(allow_file.read_text()) if allow_file.exists() else []
    exceptions = {(item["path"], item["sha256"]) for item in allow}
    fingerprint_file = root / "config/release-private-fingerprints.json"
    private_fingerprints = {item["sha256"] for item in json.loads(fingerprint_file.read_text())} if fingerprint_file.exists() else set()
    failures = []
    for name in FORBIDDEN_ROOTS | {"docker-compose.prod.yml", "deploy.md", "OPEN_SOURCE_RELEASE_PLAN.md"}:
        if (root / name).exists():
            failures.append(f"private path present: {name}")
    literals = []
    if deny_list:
        literals = [s.strip().encode() for s in deny_list.read_text().splitlines() if s.strip() and not s.startswith("#")]
    files = list(source_files(root))
    if strict:
        selected = set(files)
        files = []
        for path in sorted(root.rglob("*")):
            if path.is_symlink():
                failures.append(f"symlink present: {path.relative_to(root)}")
            elif path.is_file():
                files.append(path)
                if path not in selected:
                    failures.append(f"path outside release allowlist: {path.relative_to(root)}")
    for path in files:
        data = path.read_bytes()
        rel = path.relative_to(root).as_posix()
        for kind, pattern in PATTERNS.items():
            for match in pattern.finditer(data):
                digest = hashlib.sha256(match.group()).hexdigest()
                if (rel, digest) not in exceptions:
                    line = data[:match.start()].count(b"\n") + 1
                    failures.append(f"{rel}:{line}: {kind} fingerprint={digest}")
        for token in set(re.findall(rb"[A-Za-z0-9_-]+", data)):
            digest = hashlib.sha256(token.lower()).hexdigest()
            if digest in private_fingerprints:
                failures.append(f"{rel}: private identifier fingerprint={digest}")
        if any(literal in data for literal in literals):
            failures.append(f"{rel}: private deny-list match")
    return files, failures


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--deny-list", type=Path)
    parser.add_argument("--strict", action="store_true", help="Inspect every file and reject excluded paths")
    args = parser.parse_args()
    files, failures = check(args.root.resolve(), args.deny_list, strict=args.strict)
    for failure in failures:
        print(failure)
    print(f"Checked {len(files)} candidate source files; {len(failures)} unresolved findings.")
    raise SystemExit(bool(failures))
