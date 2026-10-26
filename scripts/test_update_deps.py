import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
FILES = (
    "go.mod",
    "Dockerfile",
    "Makefile",
    ".github/workflows/ci.yaml",
    ".github/workflows/release.yml",
    ".github/workflows/workflow-lint.yaml",
)

# Only external commands are faked; the updater edits real fixture files.
FAKE_TOOL = """#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

tool = Path(sys.argv[0]).name
args = sys.argv[1:]
if tool == "go":
    if os.environ.get("GOTOOLCHAIN") != "auto":
        sys.exit("automatic Go toolchain selection is required")
    with open("go-commands.log", "a") as log:
        log.write(" ".join(args) + "\\n")
    if args == ["build", "./..."] and os.environ.get("FAIL_GO_BUILD"):
        sys.exit("simulated Go build failure")
    if args[0] == "list" and args[-1] == "all":
        print("example.org/dependency")
    if args[:2] == ["list", "-m"]:
        module = args[-1]
        if module.startswith("sigs.k8s.io/kustomize/kustomize/v5@"):
            print("v5.99.0")
        elif module.startswith("sigs.k8s.io/controller-tools@"):
            print("v0.99.0")
    sys.exit(0)
if tool == "make":
    if args != ["manifests", "generate"]:
        sys.exit("unexpected Make invocation")
    with open("go-commands.log", "a") as log:
        log.write("make " + " ".join(args) + "\\n")
    if os.environ.get("FAIL_GENERATE"):
        sys.exit("simulated generation failure")
    sys.exit(0)
if os.environ.get("FAIL_TOOL") == tool:
    sys.exit("simulated lookup failure")
if tool == "curl":
    if args != ["--fail", "--silent", "--show-error", "https://go.dev/VERSION?m=text"]:
        sys.exit("unexpected Go release lookup")
    print(os.environ.get("GO_RELEASE", "go1.99.2\\ntime 2099-01-01T00:00:00Z"))
elif tool == "gh":
    repo = args[-1].removeprefix("repos/").removesuffix("/releases/latest")
    versions = {
        "golangci/golangci-lint": "v2.99.0",
        "helm/helm": "v4.99.0",
        "yannh/kubeconform": "v0.99.0",
        "rhysd/actionlint": "v1.99.0",
    }
    print(json.dumps({"tag_name": os.environ.get("RELEASE_TAG", versions[repo]),
                      "draft": False, "prerelease": False}))
elif tool == "docker":
    if args[:3] != ["buildx", "imagetools", "inspect"]:
        sys.exit("unexpected Docker invocation")
    if args[-1] not in ("ghcr.io/yannh/kubeconform:v0.99.0",
                        "rhysd/actionlint:1.99.0"):
        sys.exit("wrong image tag")
    print(os.environ.get("IMAGE_DIGEST", "sha256:" + "a" * 64))
else:
    sys.exit("unexpected command")
"""


class UpdateDepsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in (*FILES, "update-go-deps.sh"):
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / name, target)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name in ("go", "gh", "docker", "make", "curl"):
            tool = self.bin / name
            tool.write_text(FAKE_TOOL)
            tool.chmod(0o755)

    def run_updater(self, **env):
        return subprocess.run(
            ["bash", "update-go-deps.sh"],
            cwd=self.root,
            env={**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", **env},
            text=True,
            capture_output=True,
        )

    def snapshot(self):
        return {name: (self.root / name).read_text() for name in FILES}

    def test_updates_go_tools_and_matching_image_digests(self):
        before = self.snapshot()
        result = self.run_updater(GOTOOLCHAIN="local")
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = (self.root / "go-commands.log").read_text().splitlines()
        self.assertEqual(commands[-6:], [
            "mod tidy",
            "list -mod=readonly -m -f {{ if and (not .Replace) (not .Indirect) (not .Main)}}{{.Path}}{{end}} all",
            "get example.org/dependency@latest",
            "mod tidy",
            "build ./...",
            "make manifests generate",
        ])
        after = self.snapshot()
        self.assertIn("\ngo 1.99.2\n", after["go.mod"])
        self.assertIn("\nARG GO_VERSION=1.99\n", after["Dockerfile"])
        self.assertIn("KUSTOMIZE_VERSION ?= v5.99.0", after["Makefile"])
        self.assertIn("CONTROLLER_TOOLS_VERSION ?= v0.99.0", after["Makefile"])
        self.assertIn("version: v2.99.0", after[".github/workflows/ci.yaml"])
        for name in (".github/workflows/ci.yaml", ".github/workflows/release.yml"):
            self.assertIn("version: v4.99.0", after[name])
        self.assertIn(
            "ghcr.io/yannh/kubeconform:v0.99.0@sha256:" + "a" * 64,
            after[".github/workflows/ci.yaml"],
        )
        self.assertIn(
            "rhysd/actionlint:1.99.0@sha256:" + "a" * 64,
            after[".github/workflows/workflow-lint.yaml"],
        )
        # Action refs and all surrounding configuration must remain untouched.
        for name in FILES:
            old_lines = [line for line in before[name].splitlines() if "uses:" in line]
            new_lines = [line for line in after[name].splitlines() if "uses:" in line]
            self.assertEqual(old_lines, new_lines)
        result = self.run_updater()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(after, self.snapshot())

    def test_failed_or_invalid_metadata_does_not_edit_build_pins(self):
        before = self.snapshot()
        for env in (
            {"FAIL_TOOL": "curl"},
            {"FAIL_TOOL": "gh"},
            {"FAIL_TOOL": "docker"},
            {"RELEASE_TAG": ""},
            {"RELEASE_TAG": "v3.0.0-rc.1"},
            {"IMAGE_DIGEST": "not-a-digest"},
            {"GO_RELEASE": ""},
            {"GO_RELEASE": "go1.99rc1"},
            {"GO_RELEASE": "go1.99"},
        ):
            with self.subTest(env=env):
                for name, text in before.items():
                    (self.root / name).write_text(text)
                result = self.run_updater(**env)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(before, self.snapshot())

    def test_go_version_is_not_downgraded(self):
        path = self.root / "go.mod"
        path.write_text("module example.org/test\n\ngo 1.100.0\n")
        before = self.snapshot()
        result = self.run_updater()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Refusing to downgrade Go", result.stderr)
        self.assertEqual(before, self.snapshot())

    def test_missing_go_pins_fail_without_partial_updates(self):
        original = self.snapshot()
        for name, old, new in (
            ("Dockerfile", "ARG GO_VERSION=", "ARG OTHER="),
            ("go.mod", "\ngo ", "\nnot-go "),
        ):
            with self.subTest(name=name):
                for filename, text in original.items():
                    (self.root / filename).write_text(text)
                (self.root / name).write_text(original[name].replace(old, new))
                before = self.snapshot()
                result = self.run_updater()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(name, result.stderr)
                self.assertEqual(before, self.snapshot())

    def test_go_build_failure_is_not_reported_as_success(self):
        result = self.run_updater(FAIL_GO_BUILD="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("simulated Go build failure", result.stderr)
        self.assertNotIn("update completed!", result.stdout)
        self.assertNotIn(
            "make manifests generate",
            (self.root / "go-commands.log").read_text(),
        )

    def test_generation_failure_is_not_reported_as_success(self):
        result = self.run_updater(FAIL_GENERATE="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("simulated generation failure", result.stderr)
        self.assertNotIn("update completed!", result.stdout)

    def test_missing_pin_fails_without_partial_build_pin_updates(self):
        path = self.root / ".github/workflows/release.yml"
        path.write_text(path.read_text().replace("azure/setup-helm@", "other/setup-helm@"))
        before = self.snapshot()
        result = self.run_updater()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("release.yml", result.stderr)
        self.assertEqual(before, self.snapshot())


if __name__ == "__main__":
    unittest.main()
