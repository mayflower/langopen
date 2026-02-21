import importlib.util
import os
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch


def load_app_module():
    module_path = Path(__file__).resolve().parents[1] / "app.py"
    spec = importlib.util.spec_from_file_location("runtime_runner_app", module_path)
    if spec is None or spec.loader is None:
        raise RuntimeError("failed to load runtime-runner app module")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


APP = load_app_module()


class RuntimeRunnerAppTests(unittest.TestCase):
    def test_graph_target_from_file_path(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_root = Path(tmp).resolve()
            graph_file = repo_root / "src" / "graphs" / "graph_builder.py"
            graph_file.parent.mkdir(parents=True, exist_ok=True)
            graph_file.write_text("graph = object()\n", encoding="utf-8")

            normalized = APP._normalize_graph_target("./src/graphs/graph_builder.py:graph", repo_root)
            self.assertEqual(normalized, "src.graphs.graph_builder:graph")

    def test_dependency_plan_uses_subpath_requirements(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_root = Path(tmp)
            dep = repo_root / "my_agent"
            dep.mkdir(parents=True, exist_ok=True)
            (dep / "requirements.txt").write_text("langgraph\n", encoding="utf-8")
            req = APP.ExecuteRequest(run_id="run_test")

            plan = APP._dependency_install_plan(repo_root, {"dependencies": ["./my_agent"]}, req)
            self.assertEqual(len(plan), 1)
            self.assertEqual(plan[0]["kind"], "requirements")
            self.assertIn("my_agent/requirements.txt", plan[0]["install"])

    def test_dependency_plan_supports_package_specs(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_root = Path(tmp)
            req = APP.ExecuteRequest(run_id="run_test")

            plan = APP._dependency_install_plan(repo_root, {"dependencies": ["langchain-community"]}, req)
            self.assertEqual(len(plan), 1)
            self.assertEqual(plan[0]["kind"], "package")
            self.assertEqual(plan[0]["source"], "langchain-community")

    def test_langchain_shim_injects_legacy_symbols(self):
        original_modules = dict(sys.modules)
        try:
            langchain = types.ModuleType("langchain")
            agents = types.ModuleType("langchain.agents")

            def create_tool_calling_agent(*_args, **_kwargs):
                return "fallback-ok"

            agents.create_tool_calling_agent = create_tool_calling_agent
            langchain.agents = agents
            text_splitters = types.ModuleType("langchain_text_splitters")

            sys.modules["langchain"] = langchain
            sys.modules["langchain.agents"] = agents
            sys.modules["langchain_text_splitters"] = text_splitters

            actions = []
            APP._apply_langchain_shims(actions)

            self.assertIn("langchain.text_splitter", sys.modules)
            self.assertTrue(hasattr(sys.modules["langchain.agents"], "create_openai_tools_agent"))
            created = sys.modules["langchain.agents"].create_openai_tools_agent(None, None, None)
            self.assertEqual(created, "fallback-ok")
            self.assertTrue(any(item.startswith("shim:") for item in actions))
        finally:
            sys.modules.clear()
            sys.modules.update(original_modules)

    def test_missing_provider_env_preflight(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_root = Path(tmp)
            (repo_root / "requirements.txt").write_text("langchain-groq\n", encoding="utf-8")
            (repo_root / "graph.py").write_text(
                "import os\n"
                "from langchain_groq import ChatGroq\n"
                "def build():\n"
                "    os.getenv('GROQ_API_KEY')\n"
                "    return ChatGroq(model='llama-3.1-8b-instant')\n",
                encoding="utf-8",
            )
            req = APP.ExecuteRequest(run_id="run_test")
            plan = APP._dependency_install_plan(repo_root, {"dependencies": ["."]}, req)

            old_value = os.environ.pop("GROQ_API_KEY", None)
            try:
                with self.assertRaises(APP.RuntimeRunnerError) as raised:
                    APP._ensure_required_env(repo_root, plan)
                self.assertEqual(raised.exception.code, "missing_required_env")
                self.assertIn("GROQ_API_KEY", raised.exception.details.get("missing", []))
            finally:
                if old_value is not None:
                    os.environ["GROQ_API_KEY"] = old_value

    def test_preflight_does_not_require_unused_provider_dependency(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_root = Path(tmp)
            (repo_root / "requirements.txt").write_text("langchain-openai\n", encoding="utf-8")
            (repo_root / "graph.py").write_text("def build():\n    return {'ok': True}\n", encoding="utf-8")
            req = APP.ExecuteRequest(run_id="run_test")
            plan = APP._dependency_install_plan(repo_root, {"dependencies": ["."]}, req)
            self.assertEqual(APP._detect_required_env(repo_root, plan), [])

    def test_execution_environment_applies_and_restores_env_and_cwd(self):
        old_cwd = os.getcwd()
        old_value = os.environ.get("LANGOPEN_TEST_RUNTIME_ENV")
        with tempfile.TemporaryDirectory() as tmp:
            with APP._execution_environment({"LANGOPEN_TEST_RUNTIME_ENV": "set-value"}, tmp):
                self.assertEqual(os.environ.get("LANGOPEN_TEST_RUNTIME_ENV"), "set-value")
                self.assertEqual(Path(os.getcwd()).resolve(), Path(tmp).resolve())
        self.assertEqual(os.getcwd(), old_cwd)
        self.assertEqual(os.environ.get("LANGOPEN_TEST_RUNTIME_ENV"), old_value)

    def test_writable_path_self_check_uses_declared_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            p1 = Path(tmp) / "cache"
            p2 = Path(tmp) / "home"
            original = APP._writable_paths
            APP._writable_paths = lambda: [p1, p2]
            try:
                APP._assert_writable_paths()
            finally:
                APP._writable_paths = original
            self.assertTrue(p1.is_dir())
            self.assertTrue(p2.is_dir())

    def test_clone_checkout_ref_uses_branch_clone_for_named_refs(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_dir = Path(tmp) / "repo"
            with patch.object(APP.subprocess, "run") as run_mock:
                APP._clone_checkout_ref("https://github.com/langchain-ai/react-agent", "main", repo_dir)
                self.assertEqual(run_mock.call_count, 1)
                cmd = run_mock.call_args.args[0]
                self.assertEqual(cmd[:5], ["git", "clone", "--depth", "1", "--branch"])
                self.assertEqual(cmd[5], "main")

    def test_clone_checkout_ref_uses_fetch_for_commit_sha(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_dir = Path(tmp) / "repo"
            sha = "0e68628a2217072a79edbc22d0b9efbb13112da5"
            with patch.object(APP.subprocess, "run") as run_mock:
                APP._clone_checkout_ref("https://github.com/langchain-ai/react-agent", sha, repo_dir)
                self.assertEqual(run_mock.call_count, 3)
                clone_cmd = run_mock.call_args_list[0].args[0]
                fetch_cmd = run_mock.call_args_list[1].args[0]
                checkout_cmd = run_mock.call_args_list[2].args[0]
                self.assertEqual(clone_cmd[:3], ["git", "clone", "--no-checkout"])
                self.assertEqual(fetch_cmd[-1], sha)
                self.assertEqual(checkout_cmd[-2:], ["--detach", "FETCH_HEAD"])

    def test_invoke_target_falls_back_to_ainvoke_when_invoke_fails(self):
        class AsyncOnlyTarget:
            def invoke(self, *_args, **_kwargs):
                raise RuntimeError('No synchronous function provided to "call_model".')

            async def ainvoke(self, payload, configurable=None):
                return {"payload": payload, "configurable": configurable}

        req = APP.ExecuteRequest(run_id="run_test", input={"hello": "world"}, configurable={"foo": "bar"})
        result = APP._invoke_target(AsyncOnlyTarget(), req)
        self.assertEqual(result["payload"], {"hello": "world"})
        self.assertEqual(result["configurable"], {"foo": "bar"})

    def test_invoke_target_supports_async_graph_factory(self):
        class ProducedGraph:
            async def ainvoke(self, payload, configurable=None):
                return {"ok": True, "payload": payload, "configurable": configurable}

        async def factory(config):
            return ProducedGraph()

        req = APP.ExecuteRequest(run_id="run_test", input={"question": "ping"}, configurable={"model": "openai:gpt-4o-mini"})
        result = APP._invoke_target(factory, req)
        self.assertEqual(result["ok"], True)
        self.assertEqual(result["payload"], {"question": "ping"})
        self.assertEqual(result["configurable"], {"model": "openai:gpt-4o-mini"})


if __name__ == "__main__":
    unittest.main()
