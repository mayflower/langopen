from __future__ import annotations

import os
from typing import Any

from fastapi import FastAPI
from pydantic import BaseModel, Field


class ExecuteRequest(BaseModel):
    run_id: str = Field(..., min_length=1)
    thread_id: str = ""
    assistant_id: str = ""
    input: Any = None
    command: Any = None
    configurable: Any = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    checkpoint_id: str = ""


class EventItem(BaseModel):
    event: str
    data: Any


class ExecuteResponse(BaseModel):
    status: str
    output: Any = None
    error: Any = None
    events: list[EventItem] = Field(default_factory=list)


app = FastAPI(title="LangOpen Runtime Runner", version="0.1.0")


@app.get("/healthz")
def healthz() -> dict[str, str]:
    return {"status": "ok"}


@app.post("/execute", response_model=ExecuteResponse)
def execute(req: ExecuteRequest) -> ExecuteResponse:
    # Minimal runtime implementation for parity: execute payload-derived response.
    if isinstance(req.input, dict) and req.input.get("force_error"):
        return ExecuteResponse(
            status="error",
            error={"message": "forced error", "run_id": req.run_id},
            events=[EventItem(event="error", data={"message": "forced error"})],
        )

    token = os.getenv("RUNTIME_RUNNER_DEFAULT_TOKEN", "ok")
    messages = []
    if isinstance(req.input, dict):
        messages = req.input.get("messages", []) or []

    last_content = ""
    if isinstance(messages, list) and messages:
        last = messages[-1]
        if isinstance(last, dict):
            last_content = str(last.get("content", "")).strip()

    if last_content:
        token = last_content.split()[0][:64]

    output = {
        "run_id": req.run_id,
        "assistant_id": req.assistant_id,
        "thread_id": req.thread_id,
        "checkpoint_id": req.checkpoint_id,
        "message": "runtime execution complete",
    }

    events = [
        EventItem(event="token", data={"token": token}),
    ]

    return ExecuteResponse(status="success", output=output, events=events)
