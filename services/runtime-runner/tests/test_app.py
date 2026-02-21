import importlib.util
import os
import sys
import tempfile
import types
import unittest
from pathlib import Path


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


if __name__ == "__main__":
    unittest.main()
