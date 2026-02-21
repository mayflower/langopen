from __future__ import annotations

import asyncio
import contextlib
import hashlib
import importlib
import inspect
import json
import os
import pwd
import re
import subprocess
import sys
import threading
from pathlib import Path
from typing import Any

from fastapi import FastAPI
from pydantic import BaseModel, Field

# Some dependency stacks call Path.home()/expanduser and fail when the container UID
# does not exist in /etc/passwd; force a writable home in a mounted writable volume.
_DEFAULT_HOME = os.getenv("RUNTIME_RUNNER_WRITABLE_HOME", "/home/langopen").strip() or "/home/langopen"
_DEFAULT_CACHE = os.getenv("RUNTIME_RUNNER_CACHE_DIR", "/.cache").strip() or "/.cache"
os.environ.setdefault("HOME", _DEFAULT_HOME)
os.environ.setdefault("TMPDIR", "/tmp")
os.environ.setdefault("TMP", "/tmp")
os.environ.setdefault("TEMP", "/tmp")
os.environ.setdefault("XDG_CACHE_HOME", _DEFAULT_CACHE)
os.environ.setdefault("PIP_CACHE_DIR", f"{_DEFAULT_CACHE}/pip")
os.environ.setdefault("UV_CACHE_DIR", f"{_DEFAULT_CACHE}/uv")
_orig_getpwuid = pwd.getpwuid


def _safe_getpwuid(uid: int) -> pwd.struct_passwd:
    try:
        return _orig_getpwuid(uid)
    except KeyError:
        home = os.getenv("HOME", _DEFAULT_HOME)
        return pwd.struct_passwd(("langopen", "x", uid, uid, "langopen", home, "/sbin/nologin"))


pwd.getpwuid = _safe_getpwuid


class RuntimeRunnerError(Exception):
    def __init__(self, code: str, message: str, details: dict[str, Any] | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}

    def as_dict(self, run_id: str, graph_target: str) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "type": self.code,
            "message": self.message,
            "run_id": run_id,
            "graph_target": graph_target,
        }
        if self.details:
            payload["details"] = self.details
        return payload


class ExecuteRequest(BaseModel):
    run_id: str = Field(..., min_length=1)
    thread_id: str = ""
    assistant_id: str = ""
    input: Any = None
    command: Any = None
    configurable: Any = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    checkpoint_id: str = ""
    runtime_metadata: Any = None
    runtime_env: dict[str, str] = Field(default_factory=dict)


class EventItem(BaseModel):
    event: str
    data: Any


class ExecuteResponse(BaseModel):
    status: str
    output: Any = None
    error: Any = None
    events: list[EventItem] = Field(default_factory=list)


class DependencyPlanItem(BaseModel):
    kind: str
    source: str
    install: str
    path: str = ""
    files: list[str] = Field(default_factory=list)


app = FastAPI(title="LangOpen Runtime Runner", version="0.3.0")
_REQUIREMENTS_LOCKS: dict[str, threading.Lock] = {}
_REQUIREMENTS_LOCKS_GUARD = threading.Lock()
_REPO_LOCKS: dict[str, threading.Lock] = {}
_REPO_LOCKS_GUARD = threading.Lock()
_EXECUTION_ENV_LOCK = threading.Lock()


@app.get("/healthz")
def healthz() -> dict[str, str]:
    return {"status": "ok"}


def _writable_paths() -> list[Path]:
    return [
        Path("/tmp"),
        Path("/var/tmp"),
        Path(os.getenv("XDG_CACHE_HOME", _DEFAULT_CACHE)),
        Path(os.getenv("HOME", _DEFAULT_HOME)),
    ]


def _assert_writable_paths() -> None:
    for path in _writable_paths():
        path.mkdir(parents=True, exist_ok=True)
        sentinel = path / f".langopen-write-check-{os.getpid()}"
        sentinel.write_text("ok\n", encoding="utf-8")
        sentinel.unlink()


@app.on_event("startup")
def _startup_self_check() -> None:
    _assert_writable_paths()


def _as_dict(value: Any) -> dict[str, Any]:
    if isinstance(value, dict):
        return value
    return {}


def _env_bool(name: str, default: bool) -> bool:
    value = str(os.getenv(name, "")).strip().lower()
    if not value:
        return default
    return value in {"1", "true", "t", "yes", "y", "on"}


def _env_list(name: str, default: str) -> list[str]:
    raw = os.getenv(name, default)
    out = []
    for item in raw.split(","):
        text = item.strip()
        if text:
            out.append(text)
    return out


def _normalize_runtime_env(raw: Any) -> dict[str, str]:
    if not isinstance(raw, dict):
        return {}
    out: dict[str, str] = {}
    for key, value in raw.items():
        k = str(key or "").strip()
        if not k:
            continue
        out[k] = str(value if value is not None else "")
    return out


@contextlib.contextmanager
def _execution_environment(runtime_env: dict[str, str], repo_path: str) -> Any:
    old_cwd = os.getcwd()
    previous = {key: os.environ.get(key) for key in runtime_env}
    with _EXECUTION_ENV_LOCK:
        try:
            for key, value in runtime_env.items():
                os.environ[key] = value
            if repo_path:
                os.chdir(repo_path)
            yield
        finally:
            for key, value in previous.items():
                if value is None:
                    os.environ.pop(key, None)
                else:
                    os.environ[key] = value
            os.chdir(old_cwd)


def _repo_lock(key: str) -> threading.Lock:
    with _REPO_LOCKS_GUARD:
        lock = _REPO_LOCKS.get(key)
        if lock is None:
            lock = threading.Lock()
            _REPO_LOCKS[key] = lock
        return lock


def _requirements_lock(key: str) -> threading.Lock:
    with _REQUIREMENTS_LOCKS_GUARD:
        lock = _REQUIREMENTS_LOCKS.get(key)
        if lock is None:
            lock = threading.Lock()
            _REQUIREMENTS_LOCKS[key] = lock
        return lock


def _lookup(req: ExecuteRequest, *keys: str) -> str:
    cfg = _as_dict(req.configurable)
    meta = _as_dict(req.metadata)
    nested_meta = _as_dict(meta.get("metadata"))
    runtime_meta = _as_dict(req.runtime_metadata)
    all_maps = (cfg, meta, nested_meta, runtime_meta)
    for key in keys:
        for item in all_maps:
            value = str(item.get(key, "") or "").strip()
            if value:
                return value
    return ""


def _repo_url(req: ExecuteRequest) -> str:
    return _lookup(req, "repo_url") or str(os.getenv("RUNTIME_RUNNER_DEFAULT_REPO_URL", "")).strip()


def _repo_subpath(req: ExecuteRequest) -> str:
    return _lookup(req, "repo_path", "working_dir") or str(os.getenv("RUNTIME_RUNNER_DEFAULT_REPO_PATH", ".")).strip() or "."


def _git_ref(req: ExecuteRequest) -> str:
    return _lookup(req, "git_ref") or "main"


def _is_commit_ref(git_ref: str) -> bool:
    ref = str(git_ref or "").strip().lower()
    return bool(ref) and bool(re.fullmatch(r"[0-9a-f]{7,40}", ref))


def _clone_checkout_ref(repo_url: str, git_ref: str, repo_dir: Path) -> None:
    if _is_commit_ref(git_ref):
        subprocess.run(
            ["git", "clone", "--no-checkout", repo_url, str(repo_dir)],
            check=True,
            capture_output=True,
            text=True,
        )
        subprocess.run(
            ["git", "-C", str(repo_dir), "fetch", "--depth", "1", "origin", git_ref],
            check=True,
            capture_output=True,
            text=True,
        )
        subprocess.run(
            ["git", "-C", str(repo_dir), "checkout", "--detach", "FETCH_HEAD"],
            check=True,
            capture_output=True,
            text=True,
        )
        return
    subprocess.run(
        ["git", "clone", "--depth", "1", "--branch", git_ref, repo_url, str(repo_dir)],
        check=True,
        capture_output=True,
        text=True,
    )


def _refresh_checkout_ref(git_ref: str, repo_dir: Path) -> None:
    fetch_ref = git_ref
    subprocess.run(
        ["git", "-C", str(repo_dir), "fetch", "--depth", "1", "origin", fetch_ref],
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


def _resolve_repo_subpath(repo_dir: Path, repo_subpath: str) -> Path:
    text = str(repo_subpath or "").strip()
    if text in {"", "/", "."}:
        target = repo_dir
    else:
        target = (repo_dir / text).resolve()
        if not str(target).startswith(str(repo_dir)):
            raise RuntimeRunnerError("invalid_repo_path", "repo_path must stay within repository root")
    if not target.is_dir():
        raise RuntimeRunnerError("invalid_repo_path", f"repo_path does not exist: {target}")
    return target


def _materialize_repo_path(req: ExecuteRequest) -> str:
    repo_url = _repo_url(req)
    if not repo_url:
        return ""

    git_ref = _git_ref(req)
    repo_subpath = _repo_subpath(req)
    digest = hashlib.sha256(f"{repo_url}@{git_ref}".encode("utf-8")).hexdigest()
    cache_root = Path(os.getenv("RUNTIME_RUNNER_REPO_CACHE_DIR", "/tmp/langopen-repo-cache")).expanduser().resolve()
    repo_dir = cache_root / digest
    lock = _repo_lock(digest)

    with lock:
        cache_root.mkdir(parents=True, exist_ok=True)
        if not (repo_dir / ".git").is_dir():
            try:
                _clone_checkout_ref(repo_url, git_ref, repo_dir)
            except subprocess.CalledProcessError as exc:
                raise RuntimeRunnerError(
                    "repo_clone_failed",
                    "failed to clone repository",
                    {"repo_url": repo_url, "git_ref": git_ref, "stderr": (exc.stderr or "").strip()},
                ) from exc
        elif _env_bool("RUNTIME_RUNNER_GIT_REFRESH", False):
            try:
                _refresh_checkout_ref(git_ref, repo_dir)
            except subprocess.CalledProcessError as exc:
                raise RuntimeRunnerError(
                    "repo_refresh_failed",
                    "failed to refresh cached repository",
                    {"repo_url": repo_url, "git_ref": git_ref, "stderr": (exc.stderr or "").strip()},
                ) from exc

    return str(_resolve_repo_subpath(repo_dir.resolve(), repo_subpath))


def _repo_path(req: ExecuteRequest) -> str:
    repo_url = _repo_url(req)
    if repo_url:
        return _materialize_repo_path(req)

    for candidate in (
        _lookup(req, "repo_path", "working_dir"),
        str(os.getenv("RUNTIME_RUNNER_DEFAULT_REPO_PATH", "")).strip(),
    ):
        text = str(candidate or "").strip()
        if not text:
            continue
        path = Path(text).expanduser().resolve()
        if path.is_dir():
            return str(path)
    return ""


def _find_repo_root(repo_path: str) -> Path:
    if not repo_path:
        raise RuntimeRunnerError("missing_repo_path", "repo_path is required for graph execution")
    repo_dir = Path(repo_path).expanduser().resolve()
    for parent in (repo_dir, *repo_dir.parents):
        if (parent / ".git").is_dir():
            return parent
    return repo_dir


def _load_langgraph_config(repo_root: Path) -> dict[str, Any]:
    cfg_path = repo_root / "langgraph.json"
    if not cfg_path.is_file():
        return {}
    try:
        return json.loads(cfg_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise RuntimeRunnerError("invalid_langgraph_config", "failed to parse langgraph.json", {"path": str(cfg_path), "error": str(exc)}) from exc


def _supported_python_versions() -> list[str]:
    return _env_list("RUNTIME_RUNNER_SUPPORTED_PYTHON_VERSIONS", "3.11,3.12,3.13")


def _runtime_python_version(req: ExecuteRequest, cfg: dict[str, Any]) -> str:
    requested = _lookup(req, "python_version")
    if not requested:
        requested = str(cfg.get("python_version", "")).strip()
    if not requested:
        requested = str(os.getenv("RUNTIME_RUNNER_DEFAULT_PYTHON_VERSION", "3.11")).strip()
    supported = _supported_python_versions()
    if requested not in supported:
        raise RuntimeRunnerError(
            "unsupported_python_version",
            f"python_version '{requested}' is not supported",
            {"supported": supported},
        )
    # TODO: route execution to per-version runtime pools (3.11/3.12/3.13) instead of
    # single-image enforcement once multi-runtime control-plane orchestration is in place.
    runtime_version = f"{sys.version_info.major}.{sys.version_info.minor}"
    if requested != runtime_version:
        raise RuntimeRunnerError(
            "python_version_not_provisioned",
            f"python_version '{requested}' requested but runner image provides '{runtime_version}'",
            {"requested": requested, "runtime": runtime_version},
        )
    return requested


def _looks_like_path(entry: str) -> bool:
    if entry in {".", ".."}:
        return True
    if entry.startswith(("./", "../", "/", "~")):
        return True
    return "/" in entry or "\\" in entry


def _relative_path(path: Path, base: Path) -> str:
    return str(path.resolve().relative_to(base.resolve()))


def _descriptor_for_directory(entry: str, dep_dir: Path, repo_root: Path) -> dict[str, Any]:
    requirements = dep_dir / "requirements.txt"
    pyproject = dep_dir / "pyproject.toml"
    setup = dep_dir / "setup.py"

    if requirements.is_file():
        return {
            "kind": "requirements",
            "source": entry,
            "path": _relative_path(dep_dir, repo_root),
            "install": f"pip install -r {_relative_path(requirements, repo_root)}",
            "files": [_relative_path(requirements, repo_root)],
        }
    if pyproject.is_file():
        return {
            "kind": "pyproject",
            "source": entry,
            "path": _relative_path(dep_dir, repo_root),
            "install": f"pip install {_relative_path(dep_dir, repo_root)}",
            "files": [_relative_path(pyproject, repo_root)],
        }
    if setup.is_file():
        return {
            "kind": "setup",
            "source": entry,
            "path": _relative_path(dep_dir, repo_root),
            "install": f"pip install {_relative_path(dep_dir, repo_root)}",
            "files": [_relative_path(setup, repo_root)],
        }
    raise RuntimeRunnerError(
        "missing_dependency_descriptor",
        "dependency directory is missing requirements.txt, pyproject.toml, or setup.py",
        {"dependency": entry, "path": str(dep_dir)},
    )


def _dependency_install_plan(repo_root: Path, cfg: dict[str, Any], req: ExecuteRequest) -> list[dict[str, Any]]:
    deps = cfg.get("dependencies")
    if not isinstance(deps, list):
        deps = []
    entries = [str(item or "").strip() for item in deps if str(item or "").strip()]
    if not entries:
        fallback = _repo_subpath(req)
        entries = [fallback if fallback else "."]

    plan: list[dict[str, Any]] = []
    for entry in entries:
        if _looks_like_path(entry):
            dep_dir = (repo_root / entry).resolve() if not Path(entry).is_absolute() else Path(entry).resolve()
            if not str(dep_dir).startswith(str(repo_root.resolve())):
                raise RuntimeRunnerError("invalid_dependency_path", "dependency path must stay within repository root", {"dependency": entry})
            if not dep_dir.exists():
                raise RuntimeRunnerError("missing_dependency_path", "dependency path does not exist", {"dependency": entry, "path": str(dep_dir)})
            if dep_dir.is_file() and dep_dir.name == "requirements.txt":
                rel = _relative_path(dep_dir, repo_root)
                plan.append(
                    {
                        "kind": "requirements",
                        "source": entry,
                        "path": _relative_path(dep_dir.parent, repo_root),
                        "install": f"pip install -r {rel}",
                        "files": [rel],
                    }
                )
                continue
            if not dep_dir.is_dir():
                raise RuntimeRunnerError("invalid_dependency_path", "dependency path must be a directory", {"dependency": entry})
            plan.append(_descriptor_for_directory(entry, dep_dir, repo_root))
            continue

        # package spec
        plan.append(
            {
                "kind": "package",
                "source": entry,
                "path": "",
                "install": f"pip install {entry}",
                "files": [],
            }
        )

    return plan


def _plan_digest(repo_root: Path, python_version: str, plan: list[dict[str, Any]]) -> str:
    payload: dict[str, Any] = {"python_version": python_version, "plan": []}
    h = hashlib.sha256()
    for item in plan:
        files = []
        for rel in item.get("files", []):
            rel_text = str(rel).strip()
            if not rel_text:
                continue
            target = (repo_root / rel_text).resolve()
            if target.is_file():
                h.update(rel_text.encode("utf-8"))
                h.update(target.read_bytes())
                files.append(rel_text)
        payload["plan"].append(
            {
                "kind": item.get("kind", ""),
                "source": item.get("source", ""),
                "path": item.get("path", ""),
                "install": item.get("install", ""),
                "files": sorted(files),
            }
        )
    h.update(json.dumps(payload, sort_keys=True).encode("utf-8"))
    return h.hexdigest()


def _site_packages_path(venv_dir: Path) -> Path:
    pyver = f"python{sys.version_info.major}.{sys.version_info.minor}"
    return venv_dir / "lib" / pyver / "site-packages"


def _activate_site_packages(venv_dir: Path) -> None:
    site_packages = _site_packages_path(venv_dir)
    if site_packages.is_dir():
        site_path = str(site_packages)
        if site_path not in sys.path:
            sys.path.insert(0, site_path)



def _install_with_pip(pybin: Path, args: list[str]) -> None:
    try:
        subprocess.run([str(pybin), "-m", "pip", "install", "--disable-pip-version-check", *args], check=True, capture_output=True, text=True)
    except subprocess.CalledProcessError as exc:
        raise RuntimeRunnerError(
            "dependency_install_failed",
            "pip install failed",
            {
                "args": args,
                "stderr": (exc.stderr or "").strip(),
                "stdout": (exc.stdout or "").strip(),
            },
        ) from exc


def _ensure_plan_installed(repo_root: Path, python_version: str, plan: list[dict[str, Any]]) -> tuple[Path, Path]:
    digest = _plan_digest(repo_root, python_version, plan)
    cache_root = Path(os.getenv("RUNTIME_RUNNER_VENV_CACHE_DIR", "/tmp/langopen-venv-cache")).expanduser().resolve()
    venv_dir = cache_root / digest
    lock = _requirements_lock(digest)

    with lock:
        ready_marker = venv_dir / ".ready"
        pybin = venv_dir / "bin" / "python"
        if ready_marker.is_file() and pybin.is_file():
            _activate_site_packages(venv_dir)
            return venv_dir, pybin

        cache_root.mkdir(parents=True, exist_ok=True)
        if not venv_dir.exists():
            try:
                subprocess.run([sys.executable, "-m", "venv", str(venv_dir)], check=True, capture_output=True, text=True)
            except subprocess.CalledProcessError as exc:
                raise RuntimeRunnerError("venv_create_failed", "failed to create cached virtual environment", {"stderr": (exc.stderr or "").strip()}) from exc
        if not pybin.is_file():
            raise RuntimeRunnerError("venv_create_failed", "virtual environment python binary missing", {"path": str(pybin)})

        _install_with_pip(pybin, ["--upgrade", "pip", "setuptools", "wheel"])
        for item in plan:
            kind = str(item.get("kind", "")).strip()
            source = str(item.get("source", "")).strip()
            if kind == "requirements":
                files = item.get("files", [])
                if not isinstance(files, list) or len(files) == 0:
                    raise RuntimeRunnerError("dependency_install_failed", "requirements descriptor missing files", {"dependency": source})
                req_rel = str(files[0]).strip()
                if req_rel == "":
                    raise RuntimeRunnerError("dependency_install_failed", "requirements descriptor has empty file path", {"dependency": source})
                _install_with_pip(pybin, ["-r", str((repo_root / req_rel).resolve())])
            elif kind in {"pyproject", "setup"}:
                dep_path = str(item.get("path", "")).strip()
                _install_with_pip(pybin, [str((repo_root / dep_path).resolve())])
            elif kind == "package":
                _install_with_pip(pybin, [source])
            else:
                raise RuntimeRunnerError("dependency_install_failed", f"unknown dependency kind '{kind}'", {"dependency": source})

        ready_marker.write_text("ok\n", encoding="utf-8")
        _activate_site_packages(venv_dir)
        return venv_dir, pybin


def _import_available(name: str) -> bool:
    try:
        importlib.import_module(name)
        return True
    except Exception:
        return False


def _ensure_compat_profile(pybin: Path, profile: str, actions: list[str]) -> None:
    if profile != "langchain-community":
        return
    mapping = {
        "langchain": "langchain",
        "langchain_core": "langchain-core",
        "langchain_community": "langchain-community",
        "langchain_text_splitters": "langchain-text-splitters",
    }
    missing = [pkg for mod, pkg in mapping.items() if not _import_available(mod)]
    if missing:
        _install_with_pip(pybin, missing)
        actions.append(f"compat_install:{','.join(missing)}")


def _apply_langchain_shims(actions: list[str]) -> None:
    if not _env_bool("RUNTIME_RUNNER_ENABLE_COMPAT_SHIMS", True):
        return

    if "langchain.text_splitter" not in sys.modules:
        try:
            text_splitter = importlib.import_module("langchain_text_splitters")
            sys.modules["langchain.text_splitter"] = text_splitter
            actions.append("shim:langchain.text_splitter")
        except Exception:
            pass

    try:
        agents = importlib.import_module("langchain.agents")
        if not hasattr(agents, "create_openai_tools_agent"):
            def _fallback_create_openai_tools_agent(*args: Any, **kwargs: Any) -> Any:
                alt = getattr(agents, "create_tool_calling_agent", None)
                if alt is None:
                    raise RuntimeError("create_openai_tools_agent fallback unavailable")
                return alt(*args, **kwargs)

            setattr(agents, "create_openai_tools_agent", _fallback_create_openai_tools_agent)
            actions.append("shim:create_openai_tools_agent")
    except Exception:
        pass


_ENV_ACCESS_PATTERNS: list[re.Pattern[str]] = [
    re.compile(r"os\.getenv\(\s*['\"]([A-Z][A-Z0-9_]*_API_KEY)['\"]"),
    re.compile(r"os\.environ\.get\(\s*['\"]([A-Z][A-Z0-9_]*_API_KEY)['\"]"),
    re.compile(r"os\.environ\[\s*['\"]([A-Z][A-Z0-9_]*_API_KEY)['\"]\s*\]"),
]


def _scan_requirement_names(repo_root: Path, plan: list[dict[str, Any]]) -> set[str]:
    names: set[str] = set()
    for item in plan:
        if item.get("kind") == "package":
            names.add(str(item.get("source", "")).strip().lower())
            continue
        if item.get("kind") != "requirements":
            continue
        for rel in item.get("files", []):
            req_file = (repo_root / str(rel)).resolve()
            if not req_file.is_file():
                continue
            for raw_line in req_file.read_text(encoding="utf-8").splitlines():
                line = raw_line.strip()
                if not line or line.startswith("#"):
                    continue
                candidate = re.split(r"[<>=!~\[; ]", line, maxsplit=1)[0].strip().lower()
                if candidate:
                    names.add(candidate)
    return names


def _scan_repo_for_env_hints(repo_root: Path) -> tuple[set[str], set[str]]:
    explicit_keys: set[str] = set()
    count = 0
    for py_file in repo_root.rglob("*.py"):
        count += 1
        if count > 400:
            break
        try:
            text = py_file.read_text(encoding="utf-8")
        except Exception:
            continue
        for pattern in _ENV_ACCESS_PATTERNS:
            explicit_keys.update(pattern.findall(text))
    return explicit_keys, set()


def _detect_required_env(repo_root: Path, plan: list[dict[str, Any]]) -> list[str]:
    required: set[str] = set()
    explicit_keys, _ = _scan_repo_for_env_hints(repo_root)
    required.update(explicit_keys)
    return sorted(required)


def _ensure_required_env(repo_root: Path, plan: list[dict[str, Any]]) -> None:
    required = _detect_required_env(repo_root, plan)
    missing = [name for name in required if not str(os.getenv(name, "")).strip()]
    if missing:
        raise RuntimeRunnerError(
            "missing_required_env",
            "required environment variables are missing",
            {"missing": missing},
        )


def _graph_target(req: ExecuteRequest, cfg: dict[str, Any]) -> str:
    explicit = _lookup(req, "graph_target", "target", "graph_id")
    graphs = cfg.get("graphs") if isinstance(cfg.get("graphs"), dict) else {}

    if explicit:
        if isinstance(graphs, dict) and explicit in graphs:
            value = str(graphs.get(explicit, "") or "").strip()
            if value:
                return value
        return explicit

    if isinstance(graphs, dict):
        for value in graphs.values():
            text = str(value or "").strip()
            if text:
                return text

    default_target = str(os.getenv("RUNTIME_RUNNER_DEFAULT_TARGET", "")).strip()
    if default_target:
        return default_target
    return ""


def _normalize_graph_target(target: str, repo_root: Path) -> str:
    if ":" not in target:
        if "." in target:
            module_name, attr = target.rsplit(".", 1)
            if module_name and attr:
                return f"{module_name}:{attr}"
        raise RuntimeRunnerError("graph_target_resolution_failed", "graph target must use module:object or path.py:object", {"target": target})

    module_or_path, attr = target.split(":", 1)
    module_or_path = module_or_path.strip()
    attr = attr.strip()
    if not module_or_path or not attr:
        raise RuntimeRunnerError("graph_target_resolution_failed", "graph target is missing module/path or object", {"target": target})

    if module_or_path.endswith(".py") or module_or_path.startswith(("./", "../", "/")) or "/" in module_or_path:
        candidate = Path(module_or_path)
        if not candidate.is_absolute():
            candidate = (repo_root / candidate).resolve()
        if not str(candidate).startswith(str(repo_root.resolve())):
            raise RuntimeRunnerError("graph_target_resolution_failed", "graph target file path must stay within repository root", {"target": target})
        if not candidate.is_file():
            raise RuntimeRunnerError("graph_target_resolution_failed", "graph target file does not exist", {"target": target, "path": str(candidate)})
        rel = candidate.relative_to(repo_root)
        module_name = str(rel.with_suffix("")).replace("/", ".")
        return f"{module_name}:{attr}"

    return f"{module_or_path}:{attr}"


def _resolve_target(target: str, repo_root: Path, repo_path: str) -> Any:
    normalized = _normalize_graph_target(target, repo_root)
    module_name, attr_path = normalized.split(":", 1)

    repo_dir = Path(repo_path).expanduser().resolve() if repo_path else repo_root
    for candidate in (str(repo_root), str(repo_dir)):
        if candidate not in sys.path:
            sys.path.insert(0, candidate)

    try:
        module = importlib.import_module(module_name)
    except Exception as exc:
        raise RuntimeRunnerError("graph_target_resolution_failed", "failed to import graph module", {"target": target, "module": module_name, "error": str(exc)}) from exc

    obj: Any = module
    for segment in attr_path.split("."):
        seg = segment.strip()
        if not seg:
            continue
        if not hasattr(obj, seg):
            raise RuntimeRunnerError("graph_target_resolution_failed", "resolved module is missing target attribute", {"target": target, "attribute": seg})
        obj = getattr(obj, seg)
    return obj


def _invoke_target(target_obj: Any, req: ExecuteRequest) -> Any:
    payload = req.input
    cfg = _as_dict(req.configurable)

    def _invoke_runnable(obj: Any) -> Any:
        try:
            return obj.invoke(payload, configurable=cfg)
        except TypeError:
            return obj.invoke(payload)

    def _invoke_runnable_async(obj: Any) -> Any:
        try:
            coro = obj.ainvoke(payload, configurable=cfg)
        except TypeError:
            coro = obj.ainvoke(payload)
        return asyncio.run(coro)

    if hasattr(target_obj, "invoke") and callable(target_obj.invoke):
        try:
            result = _invoke_runnable(target_obj)
        except Exception:
            if hasattr(target_obj, "ainvoke") and callable(target_obj.ainvoke):
                result = _invoke_runnable_async(target_obj)
            else:
                raise
    elif hasattr(target_obj, "ainvoke") and callable(target_obj.ainvoke):
        result = _invoke_runnable_async(target_obj)
    elif callable(target_obj):
        signature: inspect.Signature | None = None
        try:
            signature = inspect.signature(target_obj)
        except Exception:
            signature = None

        def _binds(args: tuple[Any, ...], kwargs: dict[str, Any]) -> bool:
            if signature is None:
                return True
            try:
                signature.bind(*args, **kwargs)
                return True
            except TypeError:
                return False

        candidate_calls: list[tuple[tuple[Any, ...], dict[str, Any]]] = []
        # Prefer config-oriented graph factories first (e.g. async graph(config)).
        for key in ("config", "configurable", "runnable_config", "cfg"):
            candidate_calls.append(((), {key: cfg}))
        candidate_calls.append(((cfg,), {}))
        candidate_calls.append(((), {}))
        # Backward-compatible payload call variants.
        candidate_calls.append(((payload, cfg), {}))
        candidate_calls.append(((payload,), {}))
        candidate_calls.append(((), {"input": payload, "configurable": cfg}))

        result = None
        invoked = False
        for args, kwargs in candidate_calls:
            if not _binds(args, kwargs):
                continue
            invoked = True
            try:
                result = target_obj(*args, **kwargs)
                break
            except TypeError:
                if signature is None:
                    continue
                raise
        if not invoked:
            raise RuntimeRunnerError("graph_target_resolution_failed", "resolved graph target callable signature is unsupported")
        if inspect.isawaitable(result):
            result = asyncio.run(result)
        # Support graph-factory targets that return a runnable graph instance.
        if hasattr(result, "invoke") and callable(getattr(result, "invoke", None)):
            try:
                return _invoke_runnable(result)
            except Exception:
                if hasattr(result, "ainvoke") and callable(getattr(result, "ainvoke", None)):
                    return _invoke_runnable_async(result)
                raise
        if hasattr(result, "ainvoke") and callable(getattr(result, "ainvoke", None)):
            return _invoke_runnable_async(result)
    else:
        raise RuntimeRunnerError("graph_target_resolution_failed", "resolved graph target is not callable")

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
            error={"type": "forced_error", "message": "forced error", "run_id": req.run_id},
            events=[EventItem(event="error", data={"message": "forced error"})],
        )

    compat_actions: list[str] = []
    repo_path = ""
    graph_target = ""

    try:
        repo_path = _repo_path(req)
        repo_root = _find_repo_root(repo_path)
        langgraph_cfg = _load_langgraph_config(repo_root)
        runtime_env = _normalize_runtime_env(req.runtime_env)

        with _execution_environment(runtime_env, repo_path):
            graph_target = _graph_target(req, langgraph_cfg)
            if not graph_target:
                raise RuntimeRunnerError("graph_target_resolution_failed", "graph target is required")

            python_version = _runtime_python_version(req, langgraph_cfg)
            plan = _dependency_install_plan(repo_root, langgraph_cfg, req)
            _, pybin = _ensure_plan_installed(repo_root, python_version, plan)

            compat_profile = str(os.getenv("RUNTIME_RUNNER_COMPAT_PROFILE", "langchain-community")).strip()
            _ensure_compat_profile(pybin, compat_profile, compat_actions)
            _apply_langchain_shims(compat_actions)

            _ensure_required_env(repo_root, plan)

            try:
                target_obj = _resolve_target(graph_target, repo_root, repo_path)
            except RuntimeRunnerError as target_err:
                # Best-effort compat retry for dynamic asyncpg import failures.
                if target_err.code != "graph_target_resolution_failed":
                    raise
                target_err_text = str(target_err.details.get("error", ""))
                if "asyncpg" in target_err_text:
                    _install_with_pip(pybin, ["asyncpg"])
                    compat_actions.append("compat_install:asyncpg")
                    target_obj = _resolve_target(graph_target, repo_root, repo_path)
                elif "cannot import name 'Converter' from 'attr'" in target_err_text:
                    _install_with_pip(pybin, ["attrs>=24.2.0"])
                    compat_actions.append("compat_install:attrs>=24.2.0")
                    target_obj = _resolve_target(graph_target, repo_root, repo_path)
                else:
                    raise

            raw_result = _invoke_target(target_obj, req)

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

        if compat_actions:
            events.insert(0, EventItem(event="runtime_compat_applied", data={"actions": compat_actions}))

        if not events:
            events = [EventItem(event="token", data={"token": _token_from_input(req.input)})]

        return ExecuteResponse(status=status, output=output, error=error, events=events)
    except RuntimeRunnerError as exc:
        err_payload = exc.as_dict(req.run_id, graph_target)
        return ExecuteResponse(
            status="error",
            error=err_payload,
            events=[EventItem(event="error", data=err_payload)],
        )
    except Exception as exc:
        err_payload = {
            "type": "runtime_error",
            "message": str(exc),
            "run_id": req.run_id,
            "graph_target": graph_target,
        }
        return ExecuteResponse(
            status="error",
            error=err_payload,
            events=[EventItem(event="error", data=err_payload)],
        )
