import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class ZookeeperDownloadTest(unittest.TestCase):
    def run_download(self, available_url, empty=False):
        dockerfile = (ROOT / "docker/zookeeper-image/Dockerfile").read_text()
        helper = "ddist() {" + dockerfile.split("ddist() {", 1)[1].split(
            "    ddist ", 1
        )[0]
        helper = helper.replace("\\\n", "")
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run(
                ["sh", "-ec",
                 'wget() {\n'
                 '    while [ "$#" -gt 0 ]; do\n'
                 '        case "$1" in\n'
                 '            -O) shift; output="$1" ;;\n'
                 '            https://*) url="$1" ;;\n'
                 '        esac\n'
                 '        shift\n'
                 '    done\n'
                 '    [ "$url" = "$AVAILABLE_URL" ] || return 4\n'
                 '    : > "$output"\n'
                 '    [ "$EMPTY" = 1 ] || printf artifact > "$output"\n'
                 '}\n' + helper + '\nddist artifact.tgz zookeeper/release.tgz'],
                cwd=directory,
                env={**os.environ, "AVAILABLE_URL": available_url,
                     "EMPTY": "1" if empty else "0"},
                text=True, capture_output=True,
            )
            target = Path(directory) / "artifact.tgz"
            content = target.read_text() if target.exists() else None
        return result, content

    def test_downloads_from_cdn_when_origin_is_unavailable(self):
        result, content = self.run_download(
            "https://dlcdn.apache.org/zookeeper/release.tgz"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(content, "artifact")

    def test_falls_back_to_archive_for_older_releases(self):
        result, content = self.run_download(
            "https://archive.apache.org/dist/zookeeper/release.tgz"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(content, "artifact")

    def test_fails_when_all_sources_are_unavailable_or_empty(self):
        for empty in (False, True):
            with self.subTest(empty=empty):
                result, _ = self.run_download(
                    "https://archive.apache.org/dist/zookeeper/release.tgz"
                    if empty else "", empty=empty,
                )
                self.assertNotEqual(result.returncode, 0)


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
