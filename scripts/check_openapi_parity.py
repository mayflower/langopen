#!/usr/bin/env python3
import json
import pathlib
import sys


def load(path: pathlib.Path):
    with path.open("r", encoding="utf-8") as f:
        return json.load(f)


def methods(spec: dict, path: str):
    out = set()
    for k, _ in spec.get("paths", {}).get(path, {}).items():
        lk = k.lower().strip()
        if lk in {"get", "post", "put", "patch", "delete", "options", "head"}:
            out.add(lk)
    return out


def main() -> int:
    root = pathlib.Path(__file__).resolve().parents[1]
    upstream = root / "contracts" / "openapi" / "upstream-agent-server-v0.7.5.json"
    local = root / "contracts" / "openapi" / "agent-server.json"
    report_path = root / "contracts" / "openapi" / "parity-gap-report.json"

    upstream_spec = load(upstream)
    local_spec = load(local)

    missing_paths = []
    missing_methods = {}

    for p in sorted(upstream_spec.get("paths", {}).keys()):
        if p not in local_spec.get("paths", {}):
            missing_paths.append(p)
            continue
        up_methods = methods(upstream_spec, p)
        loc_methods = methods(local_spec, p)
        miss = sorted(list(up_methods - loc_methods))
        if miss:
            missing_methods[p] = miss

    report = {
        "upstream": {
            "version": upstream_spec.get("info", {}).get("version", "unknown"),
            "release_date": upstream_spec.get("info", {}).get("x-upstream-release-date", "unknown"),
        },
        "missing_paths": missing_paths,
        "missing_methods": missing_methods,
        "compatible": len(missing_paths) == 0 and len(missing_methods) == 0,
    }

    report_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")

    print(json.dumps(report, indent=2))
    if report["compatible"]:
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
