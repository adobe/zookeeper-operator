import json
import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[1]


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
