import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]


class E2eBuildTest(unittest.TestCase):
    def run_build(self, failure):
        workflow = (ROOT / ".github/workflows/e2e.yaml").read_text()
        step = re.split(r"- name: Build (?:and load )?ZooKeeper image\n", workflow, maxsplit=1)[1]
        script = textwrap.dedent(step.split("        run: |\n", 1)[1].split("      - name:", 1)[0])
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "scripts").mkdir()
            version = root / "scripts/zookeeper-version.sh"
            version.write_text("#!/bin/sh\necho 3.9.6\n")
            version.chmod(0o755)
            tools = root / "tools"
            tools.mkdir()
            docker = tools / "docker"
            docker.write_text("""#!/usr/bin/env python3
import os
from pathlib import Path
import sys
if sys.argv[1] == "build" and sys.argv[-1] == "docker":
    counter = Path("attempts")
    attempt = int(counter.read_text()) + 1 if counter.exists() else 1
    counter.write_text(str(attempt))
    failure = os.environ["BUILD_FAILURE"]
    if failure == "always" or failure == "other" or attempt == 1:
        print("compiler error" if failure == "other" else "Received status code 429 from server: Too Many Requests")
        sys.exit(42)
""")
            docker.chmod(0o755)
            sleep = tools / "sleep"
            sleep.write_text('#!/bin/sh\necho "$1" >> waits\n')
            sleep.chmod(0o755)
            result = subprocess.run(
                ["bash", "-euo", "pipefail", "-c", script],
                cwd=root,
                env={**os.environ, "PATH": f"{tools}:{os.environ['PATH']}",
                     "BUILD_FAILURE": failure, "IMAGE": "zookeeper:e2e"},
                text=True, capture_output=True,
            )
            attempts = int((root / "attempts").read_text())
            waits = (root / "waits").read_text().splitlines() if (root / "waits").exists() else []
            return result, attempts, waits

    def test_rate_limit_retries_then_succeeds(self):
        result, attempts, waits = self.run_build("once")
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertEqual((attempts, waits), (2, ["10"]))

    def test_deterministic_failure_does_not_retry(self):
        result, attempts, waits = self.run_build("other")
        self.assertEqual(result.returncode, 42, result.stderr + result.stdout)
        self.assertEqual((attempts, waits), (1, []))

    def test_exhausted_rate_limit_retries_still_fail(self):
        result, attempts, waits = self.run_build("always")
        self.assertEqual(result.returncode, 42, result.stderr + result.stdout)
        self.assertEqual((attempts, waits), (3, ["10", "20"]))


class SchemaMatrixTest(unittest.TestCase):
    def test_schema_validation_follows_updated_node_patches(self):
        workflow = (ROOT / ".github/workflows/ci.yaml").read_text()
        step = workflow.split("- name: Validate rendered manifests with kubeconform\n", 1)[1]
        script = textwrap.dedent(step.split("        run: |\n", 1)[1].split("\n  operator-image:", 1)[0])
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".github/workflows").mkdir(parents=True)
            (root / ".github/workflows/e2e.yaml").write_text(
                "node_image: kindest/node:v1.34.9@sha256:unused\n"
                "node_image: kindest/node:v1.35.9@sha256:unused\n"
            )
            tools = root / "tools"
            tools.mkdir()
            for name, script_text in (
                ("helm", "#!/bin/sh\necho '{}'\n"),
                ("docker", '#!/bin/sh\ncat >/dev/null\nprintf "%s\\n" "$*" >> versions\n'),
            ):
                tool = tools / name
                tool.write_text(script_text)
                tool.chmod(0o755)
            result = subprocess.run(
                ["bash", "-euo", "pipefail", "-c", script], cwd=root,
                env={**os.environ, "PATH": f"{tools}:{os.environ['PATH']}"},
                text=True, capture_output=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            commands = (root / "versions").read_text().splitlines()
            self.assertEqual(len(commands), 2)
            for command, version in zip(commands, ("1.34.9", "1.35.9")):
                self.assertIn(f"-kubernetes-version {version} ", command)


class UnitSuiteSelectionTest(unittest.TestCase):
    def test_make_test_includes_helpers_but_not_live_e2e_packages(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            shutil.copyfile(ROOT / "Makefile", root / "Makefile")
            (root / "go.mod").write_text("module example.org/operator\n")
            tools = root / "tools"
            tools.mkdir()
            go = tools / "go"
            go.write_text("""#!/bin/sh
case "$1" in
  list) printf '%s\\n' example.org/operator example.org/operator/pkg/test/e2e/e2eutil example.org/operator/test/e2e example.org/operator/test/e2e/nested ;;
  test) printf '%s\\n' "$@" > unit-command ;;
esac
""")
            go.chmod(0o755)
            result = subprocess.run(
                ["make", "test"], cwd=root,
                env={**os.environ, "PATH": f"{tools}:{os.environ['PATH']}"},
                text=True, capture_output=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            args = (root / "unit-command").read_text().splitlines()
            self.assertIn("example.org/operator/pkg/test/e2e/e2eutil", args)
            self.assertNotIn("example.org/operator/test/e2e", args)
            self.assertNotIn("example.org/operator/test/e2e/nested", args)


if __name__ == "__main__":
    unittest.main()
