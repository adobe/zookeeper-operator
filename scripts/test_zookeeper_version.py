import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class JavaDownloadTest(unittest.TestCase):
    def run_download(self, arch="amd64", base="https://download.oracle.com/java",
                     version="25", api_key=None, corrupt=False):
        dockerfile = (ROOT / "docker/zookeeper-image/Dockerfile").read_text()
        download = dockerfile.split("ENV JAVA_VERSION=", 1)[1].split(
            "RUN ", 1
        )[1].split('mkdir -p "$JAVA_HOME"', 1)[0]
        if download.startswith("--mount="):
            download = download.split("\\\n", 1)[1]
        download = download.replace("\\\n", "").rstrip() + "\n"
        variables = {key: value for key, value in os.environ.items()
                     if key != "JAVA_DOWNLOAD_APIKEY"}
        variables.update(
            ARCH=arch, java_version="25", java_download_version=version,
            java_download_base_url=base,
            DOWNLOAD_SHA=hashlib.sha256(
                b"wrong" if corrupt else b"artifact"
            ).hexdigest(),
        )
        if api_key is not None:
            variables["JAVA_DOWNLOAD_APIKEY"] = api_key
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run(
                ["bash", "-ec",
                 'dpkg() { printf "%s\\n" "$ARCH"; }\n'
                 'apt-mark() { :; }\n'
                 'apt-get() { :; }\n'
                 'rm() { :; }\n'
                 'wget() { return 99; }\n'
                 'curl() {\n'
                 '    python3 -c \'import json, sys; '
                 'open("downloads.jsonl", "a").write('
                 'json.dumps(sys.argv[1:]) + "\\n")\' "$@"\n'
                 '    for url; do :; done\n'
                 '    case "$url" in\n'
                 '        *.sha256) printf "%s\\n" "$DOWNLOAD_SHA" ;;\n'
                 '        *) printf artifact > openjdk.tgz ;;\n'
                 '    esac\n'
                 '}\n' + download],
                cwd=directory, env=variables, text=True, capture_output=True,
            )
            log = Path(directory) / "downloads.jsonl"
            calls = [json.loads(line) for line in log.read_text().splitlines()
                     ] if log.exists() else []
        return result, calls

    def test_oracle_defaults_preserve_both_architecture_urls(self):
        for arch, name in (("amd64", "x64"), ("arm64", "aarch64")):
            with self.subTest(arch=arch):
                result, calls = self.run_download(arch=arch)
                self.assertEqual(result.returncode, 0, result.stderr)
                url = ("https://download.oracle.com/java/25/latest/"
                       f"jdk-25_linux-{name}_bin.tar.gz")
                self.assertEqual([call[-1] for call in calls],
                                 [url, url + ".sha256"])
                self.assertNotIn("--expand-header", calls[0])

    def test_alternate_source_uses_patch_version_and_optional_authentication(self):
        for arch, name in (("amd64", "x64"), ("arm64", "aarch64")):
            for key in (None, "synthetic-download-key"):
                with self.subTest(arch=arch, authenticated=key is not None):
                    result, calls = self.run_download(
                        arch=arch, base="https://jdk.example.test/java",
                        version="25.0.1.1", api_key=key,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    url = ("https://jdk.example.test/java/25/25.0.1.1/"
                           f"jdk-25.0.1.1_linux-{name}_bin.tar.gz")
                    self.assertEqual([call[-1] for call in calls],
                                     [url, url + ".sha256"])
                    for call in calls:
                        self.assertIn("--fail", call)
                        self.assertIn("--location", call)
                        self.assertEqual("--expand-header" in call, key is not None)
                        if key is not None:
                            self.assertIn("%JAVA_DOWNLOAD_APIKEY", call)
                            self.assertIn("Authorization: Bearer {{JAVA_DOWNLOAD_APIKEY}}",
                                          call)
                            self.assertNotIn(key, " ".join(call))
                            self.assertNotIn(key, result.stderr)

    def test_checksum_mismatch_stops_the_build(self):
        result, _ = self.run_download(corrupt=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("FAILED", result.stdout)

    def test_unsupported_architecture_stops_before_downloading(self):
        result, calls = self.run_download(arch="s390x")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsupported architecture", result.stderr)
        self.assertEqual(calls, [])


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
