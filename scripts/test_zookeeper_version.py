import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class ZookeeperVersionTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ("Makefile", "docker/zu/build.gradle.kts",
                     "scripts/zookeeper-version.sh"):
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / name, target)
        self.buildfile = self.root / "docker/zu/build.gradle.kts"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        docker = self.bin / "docker"
        docker.write_text(
            "#!/usr/bin/env python3\n"
            "import json, sys\n"
            "with open('docker-commands.log', 'a') as log:\n"
            "    log.write(json.dumps(sys.argv[1:]) + '\\n')\n"
        )
        docker.chmod(0o755)

    def run_make(self):
        return subprocess.run(
            ["make", "build-zk-image", "VERSION=test", "GIT_SHA=test"],
            cwd=self.root,
            env={**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}"},
            text=True,
            capture_output=True,
        )

    def test_reads_shared_version_outside_repository_cwd(self):
        self.buildfile.write_text('val zookeeperVersion = "9.8.7"\n')
        result = subprocess.run(
            ["bash", str(self.root / "scripts/zookeeper-version.sh")],
            cwd=self.bin, text=True, capture_output=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "9.8.7\n")

    def test_make_passes_updated_version_to_server_build(self):
        self.buildfile.write_text('val zookeeperVersion = "9.8.7"\n')
        result = self.run_make()
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = [
            json.loads(line)
            for line in (self.root / "docker-commands.log").read_text().splitlines()
        ]
        self.assertIn("ZK_VERSION=9.8.7", commands[0])

    def test_invalid_version_stops_before_docker_build(self):
        for value in ("", 'val zookeeperVersion = ""\n',
                      'val zookeeperVersion = "latest"\n',
                      'val zookeeperVersion = "3.10.0-rc1"\n',
                      'val zookeeperVersion = "3.9.6"\nval zookeeperVersion = "3.10.0"\n'):
            with self.subTest(value=value):
                self.buildfile.write_text(value)
                result = self.run_make()
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "docker-commands.log").exists())
