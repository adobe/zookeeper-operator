import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]


class DeployTest(unittest.TestCase):
    def test_remote_e2e_passes_its_built_image_to_deploy(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            shutil.copyfile(ROOT / "Makefile", root / "Makefile")
            tools = root / "tools"
            tools.mkdir()
            log = root / "commands"
            for name in ("make", "docker", "go"):
                tool = tools / name
                tool.write_text('#!/bin/sh\necho "$0 $*" >> "$COMMAND_LOG"\n')
                tool.chmod(0o755)
            result = subprocess.run(
                [shutil.which("make"), "test-e2e-remote", "TEST_IMAGE=example/operator:remote",
                 "VERSION=regression"],
                cwd=root,
                env={**os.environ, "PATH": f"{tools}:{os.environ['PATH']}", "COMMAND_LOG": str(log)},
                text=True, capture_output=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("make deploy IMG=example/operator:remote", log.read_text())

    def test_release_metadata_updates_default_bundle_image(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        step = workflow.split("- name: Update Chart.yaml and values.yaml versions\n", 1)[1]
        script = step.split("        run: |\n", 1)[1].split("      - name:", 1)[0]
        if sys.platform == "darwin":
            script = script.replace("sed -i ", "sed -i '' ")
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            for name in ("charts/zookeeper-operator/Chart.yaml",
                         "charts/zookeeper-operator/values.yaml",
                         "config/manager/manager.yaml"):
                target = root / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / name, target)
            result = subprocess.run(
                ["bash", "-euo", "pipefail", "-c", textwrap.dedent(script)],
                cwd=root, env={**os.environ, "TAG": "0.2.99-regression"},
                text=True, capture_output=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('appVersion: "0.2.99-regression"',
                          (root / "charts/zookeeper-operator/Chart.yaml").read_text())
            self.assertIn("  tag: 0.2.99-regression",
                          (root / "charts/zookeeper-operator/values.yaml").read_text())
            self.assertIn("image: ghcr.io/adobe/zookeeper-operator:0.2.99-regression",
                          (root / "config/manager/manager.yaml").read_text())


if __name__ == "__main__":
    unittest.main()
