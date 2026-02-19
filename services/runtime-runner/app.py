from __future__ import annotations

import asyncio
import hashlib
import importlib
import inspect
import os
import subprocess
import sys
import threading
from pathlib import Path
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


app = FastAPI(title="LangOpen Runtime Runner", version="0.2.0")
_REQUIREMENTS_LOCKS: dict[str, threading.Lock] = {}
_REQUIREMENTS_LOCKS_GUARD = threading.Lock()
_REPO_LOCKS: dict[str, threading.Lock] = {}
_REPO_LOCKS_GUARD = threading.Lock()


@app.get("/healthz")
def healthz() -> dict[str, str]:
    return {"status": "ok"}


def _as_dict(value: Any) -> dict[str, Any]:
    if isinstance(value, dict):
        return value
    return {}


def _env_bool(name: str, default: bool) -> bool:
    value = str(os.getenv(name, "")).strip().lower()
    if not value:
        return default
    return value in {"1", "true", "t", "yes", "y", "on"}


def _graph_target(req: ExecuteRequest) -> str:
    cfg = _as_dict(req.configurable)
    meta = _as_dict(req.metadata)
    nested_meta = _as_dict(meta.get("metadata"))
    for candidate in (
        cfg.get("graph_target"),
        cfg.get("target"),
        cfg.get("graph_id"),
        meta.get("graph_target"),
        nested_meta.get("graph_target"),
        os.getenv("RUNTIME_RUNNER_DEFAULT_TARGET", ""),
    ):
        text = str(candidate or "").strip()
        if text:
            return text
    return ""


def _repo_path(req: ExecuteRequest) -> str:
    cfg = _as_dict(req.configurable)
    meta = _as_dict(req.metadata)
    nested_meta = _as_dict(meta.get("metadata"))
    for candidate in (
        cfg.get("repo_path"),
        cfg.get("working_dir"),
        meta.get("repo_path"),
        nested_meta.get("repo_path"),
        os.getenv("RUNTIME_RUNNER_DEFAULT_REPO_PATH", ""),
    ):
        text = str(candidate or "").strip()
        if not text:
            continue
        path = Path(text).expanduser().resolve()
        if path.is_dir():
            return str(path)
    cloned_path = _materialize_repo_path(req)
    if cloned_path:
        return cloned_path
    return ""


def _requirements_file(req: ExecuteRequest, repo_path: str) -> str:
    cfg = _as_dict(req.configurable)
    explicit = str(cfg.get("requirements_file") or cfg.get("requirements_path") or "").strip()
    if explicit:
        path = Path(explicit)
        if not path.is_absolute():
            path = Path(repo_path) / path
    else:
        path = Path(repo_path) / "requirements.txt"
    path = path.expanduser().resolve()
    if path.is_file():
        return str(path)
    return ""


def _site_packages_path(venv_dir: Path) -> Path:
    pyver = f"python{sys.version_info.major}.{sys.version_info.minor}"
    return venv_dir / "lib" / pyver / "site-packages"


def _activate_site_packages(venv_dir: Path) -> None:
    site_packages = _site_packages_path(venv_dir)
    if site_packages.is_dir():
        site_path = str(site_packages)
        if site_path not in sys.path:
            sys.path.insert(0, site_path)


def _requirements_lock(key: str) -> threading.Lock:
    with _REQUIREMENTS_LOCKS_GUARD:
        lock = _REQUIREMENTS_LOCKS.get(key)
        if lock is None:
            lock = threading.Lock()
            _REQUIREMENTS_LOCKS[key] = lock
        return lock


def _repo_lock(key: str) -> threading.Lock:
    with _REPO_LOCKS_GUARD:
        lock = _REPO_LOCKS.get(key)
        if lock is None:
            lock = threading.Lock()
            _REPO_LOCKS[key] = lock
        return lock


def _resolve_repo_subpath(repo_dir: Path, repo_subpath: str) -> Path:
    text = str(repo_subpath or "").strip()
    if text in {"", "/", "."}:
        target = repo_dir
    else:
        target = (repo_dir / text).resolve()
        if not str(target).startswith(str(repo_dir)):
            raise ValueError("repo_path must stay within repository root")
    if not target.is_dir():
        raise ValueError(f"repo_path does not exist: {target}")
    return target


def _materialize_repo_path(req: ExecuteRequest) -> str:
    cfg = _as_dict(req.configurable)
    repo_url = str(cfg.get("repo_url") or "").strip()
    if not repo_url:
        return ""

    git_ref = str(cfg.get("git_ref") or "main").strip() or "main"
    repo_subpath = str(cfg.get("repo_path") or ".").strip() or "."
    digest = hashlib.sha256(f"{repo_url}@{git_ref}".encode("utf-8")).hexdigest()
    cache_root = Path(os.getenv("RUNTIME_RUNNER_REPO_CACHE_DIR", "/tmp/langopen-repo-cache")).expanduser().resolve()
    repo_dir = cache_root / digest
    lock = _repo_lock(digest)

    with lock:
        cache_root.mkdir(parents=True, exist_ok=True)
        if not (repo_dir / ".git").is_dir():
            subprocess.run(
                ["git", "clone", "--depth", "1", "--branch", git_ref, repo_url, str(repo_dir)],
                check=True,
                capture_output=True,
                text=True,
            )
        elif _env_bool("RUNTIME_RUNNER_GIT_REFRESH", False):
            subprocess.run(
                ["git", "-C", str(repo_dir), "fetch", "--depth", "1", "origin", git_ref],
                check=True,
                capture_output=True,
                text=True,
            )
            subprocess.run(
                ["git", "-C", str(repo_dir), "reset", "--hard", "FETCH_HEAD"],
                check=True,
                capture_output=True,
                text=True,
            )

    return str(_resolve_repo_subpath(repo_dir.resolve(), repo_subpath))


def _ensure_requirements_installed(req_file: str) -> None:
    req_path = Path(req_file)
    req_bytes = req_path.read_bytes()
    digest = hashlib.sha256(req_bytes + f"{sys.version_info.major}.{sys.version_info.minor}".encode("utf-8")).hexdigest()
    cache_root = Path(os.getenv("RUNTIME_RUNNER_VENV_CACHE_DIR", "/tmp/langopen-venv-cache")).expanduser().resolve()
    venv_dir = cache_root / digest
    lock = _requirements_lock(digest)

    with lock:
        ready_marker = venv_dir / ".ready"
        if ready_marker.is_file():
            _activate_site_packages(venv_dir)
            return

        cache_root.mkdir(parents=True, exist_ok=True)
        if not venv_dir.exists():
            subprocess.run([sys.executable, "-m", "venv", str(venv_dir)], check=True, capture_output=True, text=True)
        pybin = venv_dir / "bin" / "python"
        subprocess.run([str(pybin), "-m", "pip", "install", "--disable-pip-version-check", "-r", str(req_path)], check=True, capture_output=True, text=True)
        ready_marker.write_text("ok\n", encoding="utf-8")
        _activate_site_packages(venv_dir)


def _prepare_python_env(req: ExecuteRequest, repo_path: str, target: str) -> None:
    require_requirements = _env_bool("RUNTIME_RUNNER_REQUIRE_REQUIREMENTS", True)
    if not repo_path:
        if target and require_requirements:
            raise ValueError("repo_path is required to resolve and install requirements")
        return

    req_file = _requirements_file(req, repo_path)
    if not req_file:
        if target and require_requirements:
            raise ValueError(f"requirements.txt not found for repo_path={repo_path}")
        return
    _ensure_requirements_installed(req_file)


def _resolve_target(target: str, repo_path: str) -> Any:
    if ":" in target:
        module_name, attr_path = target.split(":", 1)
    elif "." in target:
        module_name, attr_path = target.rsplit(".", 1)
    else:
        raise ValueError("graph target must use module:object or module.object format")

    module_name = module_name.strip()
    attr_path = attr_path.strip()
    if not module_name or not attr_path:
        raise ValueError("graph target is missing module or object")

    if repo_path and repo_path not in sys.path:
        sys.path.insert(0, repo_path)

    module = importlib.import_module(module_name)
    obj: Any = module
    for segment in attr_path.split("."):
        segment = segment.strip()
        if not segment:
            continue
        obj = getattr(obj, segment)
    return obj


def _invoke_target(target_obj: Any, req: ExecuteRequest) -> Any:
    payload = req.input
    cfg = _as_dict(req.configurable)

    if hasattr(target_obj, "invoke") and callable(target_obj.invoke):
        try:
            result = target_obj.invoke(payload, configurable=cfg)
        except TypeError:
            result = target_obj.invoke(payload)
    elif callable(target_obj):
        try:
            result = target_obj(payload, cfg)
        except TypeError:
            try:
                result = target_obj(payload)
            except TypeError:
                try:
                    result = target_obj(input=payload, configurable=cfg)
                except TypeError:
                    result = target_obj()
    else:
        raise ValueError("resolved graph target is not callable")

    if inspect.isawaitable(result):
        result = asyncio.run(result)
    return result


def _token_from_input(input_payload: Any) -> str:
    token = os.getenv("RUNTIME_RUNNER_DEFAULT_TOKEN", "ok")
    payload = _as_dict(input_payload)
    messages = payload.get("messages", [])
    if isinstance(messages, list) and messages:
        last = messages[-1]
        if isinstance(last, dict):
            content = str(last.get("content", "")).strip()
            if content:
                token = content.split()[0][:64]
    return token


def _normalize_events(raw_events: Any) -> list[EventItem]:
    events: list[EventItem] = []
    if not isinstance(raw_events, list):
        return events
    for item in raw_events:
        if not isinstance(item, dict):
            continue
        name = str(item.get("event", "")).strip()
        if not name:
            continue
        events.append(EventItem(event=name, data=item.get("data")))
    return events


@app.post("/execute", response_model=ExecuteResponse)
def execute(req: ExecuteRequest) -> ExecuteResponse:
    if isinstance(req.input, dict) and req.input.get("force_error"):
        return ExecuteResponse(
            status="error",
            error={"message": "forced error", "run_id": req.run_id},
            events=[EventItem(event="error", data={"message": "forced error"})],
        )

    target = _graph_target(req)
    repo_path = ""

    try:
        repo_path = _repo_path(req)
        _prepare_python_env(req, repo_path, target)
        if target:
            target_obj = _resolve_target(target, repo_path)
            raw_result = _invoke_target(target_obj, req)
        else:
            raw_result = {
                "status": "success",
                "output": {
                    "run_id": req.run_id,
                    "assistant_id": req.assistant_id,
                    "thread_id": req.thread_id,
                    "checkpoint_id": req.checkpoint_id,
                    "message": "runtime execution complete",
                },
                "events": [{"event": "token", "data": {"token": _token_from_input(req.input)}}],
            }

        if isinstance(raw_result, dict):
            status = str(raw_result.get("status", "success")).strip() or "success"
            output = raw_result.get("output", raw_result)
            error = raw_result.get("error")
            events = _normalize_events(raw_result.get("events"))
        else:
            status = "success"
            output = raw_result
            error = None
            events = []

        if not events:
            events = [EventItem(event="token", data={"token": _token_from_input(req.input)})]

        return ExecuteResponse(status=status, output=output, error=error, events=events)
    except Exception as exc:
        return ExecuteResponse(
            status="error",
            error={"message": str(exc), "run_id": req.run_id, "graph_target": target},
            events=[EventItem(event="error", data={"message": str(exc)})],
        )
