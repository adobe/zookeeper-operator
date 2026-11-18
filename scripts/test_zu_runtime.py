import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class ZuJavaVersionTest(unittest.TestCase):
    def run_build(self, release):
        dockerfile = (ROOT / "docker/Dockerfile").read_text()
        build = dockerfile.split("WORKDIR /zu\n", 1)[1].split(
            "\nFROM ", 1
        )[0].strip().removeprefix("RUN ")
        if build.startswith("--mount="):
            build = build.split("\\\n", 1)[1]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            jdk = root / "runtime-java/openjdk-selected"
            jdk.mkdir(parents=True)
            (jdk / "release").write_text(release)
            gradle = root / "gradlew"
            gradle.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
            gradle.chmod(0o700)
            return subprocess.run(
                ["sh", "-ec", build.replace("\\\n", "").replace(
                    "/runtime-java", str(root / "runtime-java")
                )],
                cwd=root, text=True, capture_output=True,
            )

    def test_compilation_target_follows_base_image_jdk(self):
        for release, major in (('JAVA_VERSION="21.0.13.1"\n', "21"),
                               ('JAVA_VERSION="25.0.5.1"\n', "25")):
            with self.subTest(major=major):
                result = self.run_build(release)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.splitlines(), [
                    f"-PjavaVersion={major}", "shadowJar", "--no-daemon",
                ])

    def test_missing_java_version_stops_before_gradle(self):
        result = self.run_build('IMPLEMENTOR="Oracle Corporation"\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")


class ZuRuntimeTest(unittest.TestCase):
    def run_zu(self, env=None, java_body=None):
        variables = {
            key: value for key, value in os.environ.items()
            if key not in ("ZK_CLIENT_HEAP", "CLIENT_JVMFLAGS", "ZK_OPTIONS")
        }
        variables.update(env or {})
        java_body = java_body or (
            """python3 -c 'import json, sys; print(json.dumps(sys.argv[1:]))' "$@";"""
        )
        return subprocess.run(
            ["bash", "-c",
             'source "$1"\n'
             f"java() {{ {java_body} }}\n"
             'shift\n'
             'zkRunZu "$@"',
             "test", str(ROOT / "docker/bin/zookeeperFunctions.sh"),
             "sync", "localhost:2181", "/path with spaces"],
            env=variables, text=True, capture_output=True,
        )

    def test_default_heap_and_quiet_logging_preserve_helper_arguments(self):
        result = self.run_zu()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), [
            "-Xmx256m",
            "-Dlogback.configurationFile=/conf/logback-quiet.xml",
            "-jar", "/opt/libs/zu.jar",
            "sync", "localhost:2181", "/path with spaces",
        ])

    def test_custom_heap_jvm_and_tls_options_are_forwarded(self):
        result = self.run_zu({
            "ZK_CLIENT_HEAP": "384",
            "CLIENT_JVMFLAGS": "-Xmx512m -Dcustom.option=value",
            "ZK_OPTIONS": (
                "-Dzookeeper.clientCnxnSocket=org.apache.zookeeper.ClientCnxnSocketNetty "
                "-Dzookeeper.client.secure=true"
            ),
        })
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), [
            "-Xmx384m", "-Xmx512m", "-Dcustom.option=value",
            "-Dlogback.configurationFile=/conf/logback-quiet.xml",
            "-Dzookeeper.clientCnxnSocket=org.apache.zookeeper.ClientCnxnSocketNetty",
            "-Dzookeeper.client.secure=true",
            "-jar", "/opt/libs/zu.jar",
            "sync", "localhost:2181", "/path with spaces",
        ])

    def test_helper_failure_is_not_swallowed(self):
        result = self.run_zu(java_body="return 7;")
        self.assertEqual(result.returncode, 7, result.stderr)
