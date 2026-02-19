from __future__ import annotations

from typing import Any

import pendulum


def run(input_payload: Any = None, configurable: dict[str, Any] | None = None) -> dict[str, Any]:
    now = pendulum.now("UTC").to_iso8601_string()
    return {
        "status": "success",
        "output": {
            "proof": "python-agent-executed",
            "dependency": f"pendulum-{pendulum.__version__}",
            "timestamp_utc": now,
            "input": input_payload,
            "configurable": configurable or {},
        },
        "events": [
            {
                "event": "python_proof",
                "data": {"library": "pendulum", "timestamp_utc": now},
            }
        ],
    }
