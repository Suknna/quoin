#!/usr/bin/env python3
"""Prepare the next release's pinned defaults from the latest reachable tag."""

import argparse
import re
import subprocess
import sys
from pathlib import Path


VERSION = re.compile(r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
ROOT = Path(__file__).resolve().parent.parent

# Only maintained release defaults change. Historical references and dependency
# versions must never be included in a repository-wide version replacement.
PINNED_FILES = {
    "Makefile": 1,
    "README.md": 1,
    "README.en.md": 1,
    "deploy/compose.yaml": 4,
    "deploy/e2e-real.compose.yaml": 4,
    "deploy/images/build.sh": 1,
    "deploy/images/plinth/Dockerfile": 1,
    "deploy/images/quoin/Dockerfile": 1,
    "deploy/images/stele/Dockerfile": 1,
    "deploy/kubernetes/manifest_test.go": 1,
    "deploy/kubernetes/quoin-nginx.yaml": 4,
    "deploy/kubernetes/quoin.yaml": 4,
    "docs/getting-started-kubernetes.md": 5,
    "docs/getting-started.md": 3,
}


def latest_version(tags: list[str]) -> str:
    versions = [
        tuple(map(int, match.groups()))
        for tag in tags
        if (match := VERSION.fullmatch(tag)) is not None
    ]
    if not versions:
        raise ValueError("no reachable vX.Y.Z release tag found")
    return "v" + ".".join(map(str, max(versions)))


def next_version(current: str, bump: str) -> str:
    match = VERSION.fullmatch(current)
    if match is None:
        raise ValueError(f"invalid release version: {current}")
    major, minor, patch = map(int, match.groups())
    if bump == "fix":
        patch += 1
    elif bump == "feature":
        minor, patch = minor + 1, 0
    else:
        raise ValueError(f"unsupported release type: {bump}")
    return f"v{major}.{minor}.{patch}"


def update_defaults(root: Path, current: str, target: str) -> None:
    changes = {}
    for name, expected in PINNED_FILES.items():
        path = root / name
        content = path.read_text(encoding="utf-8")
        actual = content.count(current)
        if actual != expected:
            raise ValueError(f"{name}: expected {expected} copies of {current}, found {actual}")
        changes[path] = content.replace(current, target)

    # Validate every file before writing any, so a drifted pin cannot leave a
    # half-updated release commit in the working tree.
    for path, content in changes.items():
        path.write_text(content, encoding="utf-8")


def defaults_already_prepared(root: Path, current: str, target: str) -> bool:
    return all(
        (content := (root / name).read_text(encoding="utf-8")).count(target) == expected
        and current not in content
        for name, expected in PINNED_FILES.items()
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("bump", choices=("fix", "feature"))
    args = parser.parse_args()
    tags = subprocess.check_output(
        ["git", "tag", "--merged", "HEAD", "--list", "v*"], cwd=ROOT, text=True
    ).splitlines()
    current = latest_version(tags)
    target = next_version(current, args.bump)
    tagged_commit = subprocess.check_output(
        ["git", "rev-parse", f"refs/tags/{current}^{{}}"], cwd=ROOT, text=True
    ).strip()
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    if tagged_commit == head:
        raise ValueError(f"main has no commits since {current}; nothing new to release")
    if subprocess.run(
        ["git", "show-ref", "--verify", "--quiet", f"refs/tags/{target}"],
        cwd=ROOT,
        check=False,
    ).returncode == 0:
        raise ValueError(f"release tag already exists: {target}")
    subject = subprocess.check_output(
        ["git", "log", "-1", "--format=%s"], cwd=ROOT, text=True
    ).strip()
    if not (subject == f"release: prepare {target}" and defaults_already_prepared(ROOT, current, target)):
        update_defaults(ROOT, current, target)
    print(target)


if __name__ == "__main__":
    try:
        main()
    except ValueError as error:
        sys.exit(f"release preparation: {error}")
