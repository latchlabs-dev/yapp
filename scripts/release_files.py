"""Allowlisted source selection shared by release checks and local packaging."""
from pathlib import Path

FILES = {
    "AGENTS.md", "README.md", "CONTRIBUTING.md", "SECURITY.md", "LICENSE",
    "NOTICE", "THIRD_PARTY_NOTICES.md", "Makefile", "Dockerfile", "dev.sh",
    "go.mod", "go.sum", "docker-compose.yml", ".env.example", ".dockerignore",
    ".gitignore", ".editorconfig", ".golangci.yml", ".npmrc",
}
DIRECTORIES = {
    "cmd", "internal", "migrations", "extensions", "services", "third_party",
    "ui", "webui", "config", "scripts", "docs", "examples", ".github", ".githooks",
    "testdata",
}
EXCLUDED = {".git", "node_modules", "dist", "bin", "__pycache__", ".venv", ".private", "secrets", "data", ".ds-css", ".ds-fonts"}


def source_files(root: Path):
    for path in sorted(root.rglob("*")):
        rel = path.relative_to(root)
        # internal/secrets is application code, not a credential directory.
        parts = rel.parts
        if any(part in EXCLUDED and not (part == "secrets" and parts[:2] == ("internal", "secrets")) for part in parts):
            continue
        if path.is_symlink():
            raise ValueError(f"Release source cannot contain symlinks: {rel}")
        if not path.is_file():
            continue
        if len(parts) == 1 and path.name not in FILES:
            continue
        if len(parts) > 1 and parts[0] not in DIRECTORIES:
            continue
        if path.name.startswith(".env") and path.name != ".env.example":
            continue
        if path.suffix in {".pyc", ".tsbuildinfo", ".key", ".pem"} or path.name.endswith(".sa.json"):
            continue
        if parts[:2] in {("docs", "journal"), ("docs", "reviews")}:
            continue
        yield path
