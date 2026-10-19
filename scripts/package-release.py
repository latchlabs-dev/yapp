#!/usr/bin/env python3
"""Build local release archives. No registry login, push, or GitHub publication.

A missing project LICENSE blocks releases. --rehearsal permits private test
artifacts explicitly marked ineligible for distribution.
"""
import argparse
import gzip
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

from dependency_licenses import collect

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("release_check", ROOT / "scripts/check-release.py")
checks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checks)


def archive(path, entries):
    with path.open("wb") as raw, gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as output:
            for name, data, mode in entries:
                info = tarfile.TarInfo(name)
                info.size, info.mode, info.mtime = len(data), mode, 0
                output.addfile(info, io.BytesIO(data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    parser.add_argument("--platform", action="append", choices=["linux/amd64", "linux/arm64"])
    parser.add_argument("--rehearsal", action="store_true")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9._-]+", args.version):
        parser.error("version must contain only letters, digits, dots, underscores or hyphens")
    licensed = (ROOT / "LICENSE").is_file()
    if not licensed and not args.rehearsal:
        parser.error("project license selection is pending; only --rehearsal is allowed")
    files, failures = checks.check(ROOT)
    if failures:
        raise SystemExit("Source checks failed:\n" + "\n".join(failures))
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT))
    # Freeze file bytes once; build from this isolated snapshot, never from a
    # working tree that another contributor may change during a long build.
    contents = [(p.relative_to(ROOT).as_posix(), p.read_bytes(), 0o755 if os.access(p, os.X_OK) else 0o644) for p in files]
    snapshot_context = tempfile.TemporaryDirectory(prefix="piproxy-source-")
    snapshot = Path(snapshot_context.name)
    for rel, data, mode in contents:
        target = snapshot / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        target.chmod(mode)
    _, snapshot_failures = checks.check(snapshot, strict=True)
    if snapshot_failures:
        raise SystemExit("Frozen source checks failed:\n" + "\n".join(snapshot_failures))
    manifest = {rel: hashlib.sha256(data).hexdigest() for rel, data, _ in contents}
    subprocess.run(["go", "mod", "download"], cwd=snapshot, check=True)
    dependencies, license_entries = collect(snapshot)
    dependency_bytes = (json.dumps(dependencies, indent=2) + "\n").encode()
    license_entries.append(("DEPENDENCIES.json", dependency_bytes, 0o644))
    identity = {"version": args.version, "source_commit": commit, "working_tree_modified": dirty,
                "distribution_eligible": licensed and not args.rehearsal, "files": manifest}
    identity_bytes = (json.dumps(identity, indent=2, sort_keys=True) + "\n").encode()
    label = f"piproxy-{args.version}"
    source = output / f"{label}-source.tar.gz"
    archives = [source]
    archive(source, [(f"{label}/{rel}", data, mode) for rel, data, mode in contents]
            + [(f"{label}/SOURCE_MANIFEST.json", identity_bytes, 0o644)]
            + [(f"{label}/{name}", data, mode) for name, data, mode in license_entries])
    with tempfile.TemporaryDirectory(prefix="piproxy-package-") as tmp:
        for platform in args.platform or ["linux/amd64", "linux/arm64"]:
            goos, goarch = platform.split("/")
            env = dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=goarch)
            entries = []
            for name in ["proxy", "admin", "pdfextract", "sessionarchive", "subscription-reconcile"]:
                binary = Path(tmp) / name
                flags = f"-X github.com/earlgrey/piproxy/internal/config.Version={args.version} -X github.com/earlgrey/piproxy/internal/config.Commit={commit}"
                subprocess.run(["go", "build", "-trimpath", "-ldflags", flags, "-o", str(binary), f"./cmd/{name}"], cwd=snapshot, env=env, check=True)
                entries.append((f"bin/{name}", binary.read_bytes(), 0o755))
            for rel, data, _ in contents:
                if rel.startswith("migrations/") or rel in {"extensions/corp-proxy.ts", "LICENSE", "THIRD_PARTY_NOTICES.md", "third_party/duffel/LICENSE"}:
                    entries.append((rel, data, 0o644))
            entries.append(("SOURCE_MANIFEST.json", identity_bytes, 0o644))
            entries.extend(license_entries)
            binary_archive = output / f"{label}-{goos}-{goarch}.tar.gz"
            archive(binary_archive, entries)
            archives.append(binary_archive)
    (output / "SOURCE_MANIFEST.json").write_bytes(identity_bytes)
    (output / "DEPENDENCIES.json").write_bytes(dependency_bytes)
    (output / "SHA256SUMS").write_text("".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in archives))
    snapshot_context.cleanup()
    print(f"Built {len(archives)} local archives; distribution_eligible={identity['distribution_eligible']}")


if __name__ == "__main__":
    main()
