import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
GENERATOR = """#!/bin/sh
if [ "$1" = "--version" ]; then
    echo "Version: VERSION"
else
    echo "VERSION $*" >> "$GENERATOR_LOG"
fi
"""
FAKE_GO = """#!/usr/bin/env python3
import os
from pathlib import Path
import sys

if sys.argv[1] == "env":
    sys.exit(0)
if sys.argv[1:] != [
    "install", "sigs.k8s.io/controller-tools/cmd/controller-gen@v0.99.0"
]:
    sys.exit("unexpected Go invocation")
if os.environ.get("FAIL_INSTALL"):
    sys.exit("simulated install failure")
binary = Path(os.environ["GOBIN"]) / "controller-gen"
binary.write_text(os.environ["NEW_GENERATOR"])
binary.chmod(0o755)
"""


class ControllerGenTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        shutil.copyfile(ROOT / "Makefile", self.root / "Makefile")
        self.tools = self.root / "tools"
        self.tools.mkdir()
        go = self.tools / "go"
        go.write_text(FAKE_GO)
        go.chmod(0o755)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.binary = self.bin / "controller-gen"
        self.log = self.root / "generator.log"

    def write_generator(self, version):
        self.binary.write_text(GENERATOR.replace("VERSION", version))
        self.binary.chmod(0o755)

    def run_make(self, target="controller-gen", **env):
        return subprocess.run(
            ["make", target, "CONTROLLER_TOOLS_VERSION=v0.99.0"],
            cwd=self.root,
            env={
                **os.environ,
                "PATH": f"{self.tools}:{os.environ['PATH']}",
                "GENERATOR_LOG": str(self.log),
                "NEW_GENERATOR": GENERATOR.replace("VERSION", "v0.99.0"),
                **env,
            },
            text=True,
            capture_output=True,
        )

    def test_installs_missing_generator_and_upgrades_stale_generator(self):
        for version in (None, "v0.19.0", "v0.99.0-extra"):
            with self.subTest(version=version):
                if version is not None:
                    self.write_generator(version)
                result = self.run_make()
                self.assertEqual(result.returncode, 0, result.stderr)
                version_result = subprocess.check_output(
                    [str(self.binary), "--version"], text=True
                )
                self.assertEqual(version_result, "Version: v0.99.0\n")

    def test_matching_generator_is_reused(self):
        self.write_generator("v0.99.0")
        result = self.run_make(FAIL_INSTALL="1")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_install_failure_is_reported(self):
        self.write_generator("v0.19.0")
        result = self.run_make(FAIL_INSTALL="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("simulated install failure", result.stderr)

    def test_generate_checks_version_before_running_generator(self):
        self.write_generator("v0.19.0")
        bases = self.root / "config/crd/bases"
        bases.mkdir(parents=True)
        (bases / "zookeeper.pravega.io_zookeeperclusters.yaml").write_text("---\n")
        (self.root / "charts/zookeeper-operator/templates").mkdir(parents=True)
        result = self.run_make("generate")
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.log.read_text().splitlines()
        self.assertTrue(calls[0].startswith("v0.99.0 object"), calls)
        self.assertTrue(all(call.startswith("v0.99.0 ") for call in calls), calls)


if __name__ == "__main__":
    unittest.main()
