#!/usr/bin/env python3
"""Temporary audit: does every V2 handler read the params its pattern declares?

A route like `/admin/users/:uid/emby` whose handler reads params["id"] compiles,
registers and returns 404 for every request. Nothing in the type system or the
route table can see it, so it has to be checked by reading both sides.
"""

import pathlib
import re
import sys

ROOT = pathlib.Path(".")
API_DIR = ROOT / "internal/api"

ROUTE_RE = re.compile(
    r'a\.add\(http\.Method(\w+),\s*"([^"]+)",\s*Auth\w+,\s*(?:a\.withAPIKeyPermission\([^,]+,\s*)?a\.(\w+)\)'
)
HANDLER_RE = re.compile(r"func \(a \*App\) (\w+)\(")
PARAM_READ_RE = re.compile(r'params\["([^"]+)"\]')


def handler_bodies() -> dict[str, str]:
    bodies: dict[str, str] = {}
    for file in sorted(API_DIR.glob("*.go")):
        if file.name.endswith("_test.go"):
            continue
        lines = file.read_text(encoding="utf-8").splitlines()
        for index, line in enumerate(lines):
            match = HANDLER_RE.search(line)
            if not match:
                continue
            end = index + 1
            while end < len(lines) and lines[end] != "}":
                end += 1
            bodies[match.group(1)] = "\n".join(lines[index:end])
    return bodies


def pattern_params(pattern: str) -> set[str]:
    return {p[1:] for p in pattern.split("/") if p.startswith(":")}


def main() -> int:
    bodies = handler_bodies()
    problems = []
    checked = 0
    for file in sorted(API_DIR.glob("routes*.go")):
        text = file.read_text(encoding="utf-8")
        for _method, pattern, handler in ROUTE_RE.findall(text):
            body = bodies.get(handler)
            if body is None:
                problems.append((pattern, handler, "handler body not found"))
                continue
            declared = pattern_params(pattern)
            read = set(PARAM_READ_RE.findall(body))
            checked += 1
            missing = read - declared
            if missing:
                problems.append((pattern, handler, f"reads {sorted(missing)}, pattern declares {sorted(declared)}"))

    print(f"routes checked: {checked}")
    if not problems:
        print("no param drift")
        return 0
    print(f"\n{len(problems)} route/handler param mismatches:")
    for pattern, handler, why in problems:
        print(f"  {pattern}\n      {handler}: {why}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
