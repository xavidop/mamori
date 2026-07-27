#!/usr/bin/env python3
"""Assert that every Go module in the repo has a gomod entry in dependabot.yml.

.github/dependabot.yml is hand-maintained while CI discovers modules
dynamically, so the two drift apart silently: a new module gets built, tested,
and linted from day one but never receives a dependency update. This check
closes that gap by failing CI instead.

Usage:
    check_dependabot_coverage.py            # discover modules itself
    check_dependabot_coverage.py '["...", ...]'   # take the CI discover job's JSON list
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import yaml

REPO_ROOT = Path(__file__).resolve().parents[2]
DEPENDABOT = REPO_ROOT / ".github" / "dependabot.yml"


def discover_modules() -> list[str]:
    """Find every Go module, matching the CI discover job's find invocation."""
    out = subprocess.run(
        [
            "find", ".", "-name", "go.mod",
            "-not", "-path", "./site/*",
            "-not", "-path", "./**/testdata/*",
            "-exec", "dirname", "{}", ";",
        ],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    return sorted(line for line in out.splitlines() if line)


def as_dependabot_dir(module_dir: str) -> str:
    """Map a find-style module path onto dependabot's `directory` convention."""
    if module_dir == ".":
        return "/"
    if module_dir.startswith("./"):
        return module_dir[1:]
    return "/" + module_dir.lstrip("/")


def main() -> int:
    modules = json.loads(sys.argv[1]) if len(sys.argv) > 1 else discover_modules()

    config = yaml.safe_load(DEPENDABOT.read_text())
    covered = {
        update["directory"]
        for update in config.get("updates", [])
        if update.get("package-ecosystem") == "gomod"
    }

    missing = [
        (module, as_dependabot_dir(module))
        for module in modules
        if as_dependabot_dir(module) not in covered
    ]

    # The reverse direction matters too: an entry left behind after a module is
    # renamed or removed makes dependabot log an error on every run.
    stale = sorted(covered - {as_dependabot_dir(m) for m in modules})

    for module, directory in missing:
        print(
            f'::error file=.github/dependabot.yml::Go module {module} has no gomod '
            f'entry in dependabot.yml (expected directory: "{directory}")'
        )
    for directory in stale:
        print(
            f'::error file=.github/dependabot.yml::gomod entry directory: "{directory}" '
            f"does not correspond to any Go module in the repo"
        )

    if missing or stale:
        print(f"\n{len(missing)} missing, {len(stale)} stale, {len(modules)} modules total")
        return 1

    print(f"All {len(modules)} Go modules are covered by dependabot.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
