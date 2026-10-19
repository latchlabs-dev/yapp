"""Collect dependency metadata and available license texts for local artifacts."""
import json
from pathlib import Path
import re
import subprocess


def collect(root):
    inventory, entries, seen = [], [], set()

    def record(ecosystem, name, version, directory, declared=None):
        key = (ecosystem, name, version)
        if key in seen:
            return
        seen.add(key)
        slug = re.sub(r"[^A-Za-z0-9._-]", "_", f"{name}-{version}")
        notices = []
        if directory:
            for p in sorted(directory.iterdir()):
                if p.is_file() and p.name.upper().startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE")):
                    target = f"licenses/{ecosystem}/{slug}/{p.name}"
                    entries.append((target, p.read_bytes(), 0o644))
                    notices.append(target)
        inventory.append({"ecosystem": ecosystem, "name": name, "version": version,
                          "declared_license": declared, "license_files": notices,
                          "license_text_status": "collected" if notices else "not_collected"})

    raw = subprocess.check_output(["go", "list", "-m", "-json", "all"], cwd=root, text=True)
    decoder = json.JSONDecoder()
    while raw.strip():
        module, consumed = decoder.raw_decode(raw.lstrip())
        raw = raw.lstrip()[consumed:]
        if module.get("Main"):
            continue
        resolved = module.get("Replace", module)
        directory = Path(resolved["Dir"]) if resolved.get("Dir") else None
        record("go", module["Path"], module.get("Version", "local"), directory)

    for project in ("ui", "webui", "extensions/pi-terminal-browser"):
        store = root / project / "node_modules/.pnpm"
        for path in sorted(store.glob("*/node_modules/*/package.json")) + sorted(store.glob("*/node_modules/@*/*/package.json")):
            if path.parent.is_symlink():
                continue
            data = json.loads(path.read_text())
            record("npm", data.get("name", path.parent.name), data.get("version", "unknown"), path.parent, data.get("license"))
        # Source snapshots deliberately exclude node_modules. The frozen v9
        # lockfile is the authoritative inventory even without installed deps.
        lock = root / project / "pnpm-lock.yaml"
        if lock.exists():
            in_packages = False
            for line in lock.read_text().splitlines():
                if line == "packages:":
                    in_packages = True
                    continue
                if in_packages and line and not line.startswith(" "):
                    break
                if not in_packages or not re.match(r"^  \S.*:$", line):
                    continue
                package = line.strip()[:-1].strip("'\"")
                name, separator, version = package.rpartition("@")
                if not name or not separator or not version:
                    raise ValueError(f"Unsupported pnpm package entry in {project}")
                record("npm", name, version, None)

    # uv.lock records the complete Python package set even without a local venv.
    lock = root / "services/session-query/uv.lock"
    if lock.exists():
        for block in lock.read_text().split("[[package]]")[1:]:
            name = re.search(r'^name = "([^"]+)"', block, re.M)
            version = re.search(r'^version = "([^"]+)"', block, re.M)
            if name and version:
                record("python", name[1], version[1], None)
    inventory.sort(key=lambda item: (item["ecosystem"], item["name"], item["version"]))
    return inventory, entries
