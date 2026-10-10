#!/usr/bin/env python3
"""Require an Agent release version bump when a PR changes runtime code."""
import pathlib
import re
import subprocess
import sys


def version(raw):
    match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", raw.strip())
    if not match:
        raise ValueError("VERSION must be vMAJOR.MINOR.PATCH")
    return tuple(int(part) for part in match.groups())


def check(base):
    old_text = subprocess.check_output(["git", "show", f"{base}:VERSION"], text=True).strip()
    new_text = pathlib.Path("VERSION").read_text().strip()
    old, new = version(old_text), version(new_text)
    changed = subprocess.check_output(["git", "diff", "--name-only", base, "--"], text=True).splitlines()
    prefixes = ("cmd/codegate-agent/", "internal/agent/", "internal/terminal/", "internal/session/", "internal/protocol/", "internal/buffer/", "internal/transport/")
    runtime_changed = any(
        path in {"go.mod", "go.sum"}
        or (path.startswith(prefixes) and path.endswith(".go") and not path.endswith("_test.go"))
        for path in changed
    )
    if new < old:
        raise ValueError(f"VERSION cannot decrease: {old_text} -> {new_text}")
    if runtime_changed and new <= old:
        raise ValueError(f"Agent runtime changed but VERSION did not increase ({old_text}); bump VERSION so installed Agents can upgrade")
    print(f"Agent version check passed: {old_text} -> {new_text}")


if __name__ == "__main__":
    try:
        if len(sys.argv) != 2:
            raise ValueError("usage: check-agent-version.py BASE_COMMIT")
        check(sys.argv[1])
    except (ValueError, subprocess.CalledProcessError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        sys.exit(1)
