import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
# Keep starting pins independent of files changed by the dependency updater.
FIXTURES = {
    "go.mod": "module example.org/test\n\ngo 1.25.0\n",
    "Dockerfile": "ARG GO_VERSION=1.25\n",
    "Makefile": (
        "KUSTOMIZE_VERSION ?= v5.0.0\n"
        "CONTROLLER_TOOLS_VERSION ?= v0.19.0\n"
        "KIND_VERSION ?= v0.30.0\n"
        "ENVTEST_TOOL_VERSION = v0.0.0-20250101000000-abcdef012345\n"
        "ENVTEST_K8S_VERSION = 1.34.1\n"
    ),
    ".github/workflows/ci.yaml": (
        "- uses: golangci/golangci-lint-action@v8\n"
        "  with:\n"
        "    version: v2.0.0\n"
        "- uses: azure/setup-helm@v4\n"
        "  with:\n"
        "    version: v4.0.0\n"
        "image: ghcr.io/yannh/kubeconform:v0.6.0@sha256:" + "0" * 64 + "\n"
    ),
    ".github/workflows/release.yml": (
        "- uses: azure/setup-helm@v4\n"
        "  with:\n"
        "    version: v4.0.0\n"
    ),
    ".github/workflows/workflow-lint.yaml": (
        "image: rhysd/actionlint:1.7.0@sha256:" + "0" * 64 + "\n"
    ),
    ".github/workflows/e2e.yaml": (
        'kubernetes: "1.34"\n'
        "node_image: kindest/node:v1.34.1@sha256:" + "0" * 64 + "\n"
    ),
}

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
        elif module.startswith("sigs.k8s.io/controller-runtime/tools/setup-envtest@"):
            print("v0.0.0-20990101000000-abcdef012345")
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
    if args[-1] == "repos/kubernetes-sigs/kind/releases?per_page=100":
        minors = ("1.35",) if os.environ.get("NO_NODE_IMAGE") else ("1.34", "1.35")
        body = "\\n".join(f"kindest/node:v{minor}.9@sha256:" + "b" * 64 for minor in minors)
        print(json.dumps([[{"tag_name": "v0.99.0", "body": body}]]))
        sys.exit(0)
    if args[-1] == "repos/kubernetes-sigs/controller-tools/releases?per_page=100":
        assets = [] if os.environ.get("NO_ENVTEST_ASSETS") else [
            {"name": f"envtest-v1.34.9-{platform}.tar.gz"}
            for platform in ("linux-amd64", "darwin-arm64")
        ]
        print(json.dumps([[{"tag_name": "envtest-v1.34.9", "assets": assets}]]))
        sys.exit(0)
    repo = args[-1].removeprefix("repos/").removesuffix("/releases/latest")
    versions = {
        "golangci/golangci-lint": "v2.99.0",
        "helm/helm": "v4.99.0",
        "yannh/kubeconform": "v0.99.0",
        "rhysd/actionlint": "v1.99.0",
        "kubernetes-sigs/kind": "v0.99.0",
    }
    print(json.dumps({"tag_name": os.environ.get("RELEASE_TAG", versions[repo]),
                      "draft": False, "prerelease": False}))
elif tool == "docker":
    if args[:3] != ["buildx", "imagetools", "inspect"]:
        sys.exit("unexpected Docker invocation")
    if args[-1] not in ("ghcr.io/yannh/kubeconform:v0.99.0",
                        "rhysd/actionlint:1.99.0",
                        "kindest/node:v1.34.9", "kindest/node:v1.35.9"):
        sys.exit("wrong image tag")
    digest_char = "b" if args[-1].startswith("kindest/node:") else "a"
    print(os.environ.get("IMAGE_DIGEST", "sha256:" + digest_char * 64))
else:
    sys.exit("unexpected command")
"""


class UpdateDepsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name, text in FIXTURES.items():
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text)
        shutil.copy2(ROOT / "update-go-deps.sh", self.root / "update-go-deps.sh")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name in ("go", "gh", "docker", "make", "curl"):
            tool = self.bin / name
            tool.write_text(FAKE_TOOL)
            tool.chmod(0o755)

    def run_updater(self, **env):
        return subprocess.run(
            ["bash", "-e", "-c", "./update-go-deps.sh"],
            cwd=self.root,
            env={**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", **env},
            text=True,
            capture_output=True,
        )

    def snapshot(self):
        return {name: (self.root / name).read_text() for name in FIXTURES}

    def test_updates_go_tools_and_matching_image_digests(self):
        result = self.run_updater(GOTOOLCHAIN="local")
        self.assertEqual(result.returncode, 0, result.stderr)
        after = self.snapshot()
        self.assertIn("go 1.99.2\n", after["go.mod"])
        self.assertIn("ARG GO_VERSION=1.99\n", after["Dockerfile"])
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
        self.assertIn("KIND_VERSION ?= v0.99.0", after["Makefile"])
        self.assertIn("ENVTEST_TOOL_VERSION = v0.0.0-20990101000000-abcdef012345",
                      after["Makefile"])
        self.assertIn("ENVTEST_K8S_VERSION = 1.34.9", after["Makefile"])
        workflow = after[".github/workflows/e2e.yaml"]
        self.assertIn('kubernetes: "1.34"', workflow)
        self.assertIn("kindest/node:v1.34.9@sha256:" + "b" * 64, workflow)
        self.assertNotIn("1.35", workflow)
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
            {"NO_NODE_IMAGE": "1"},
            {"NO_ENVTEST_ASSETS": "1"},
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

    def test_missing_pins_fail_without_partial_updates(self):
        original = self.snapshot()
        for name, old, new in (
            ("Dockerfile", "ARG GO_VERSION=", "ARG OTHER="),
            ("go.mod", "\ngo ", "\nnot-go "),
            (".github/workflows/release.yml", "azure/setup-helm@", "other/setup-helm@"),
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

    def test_build_or_generation_failure_is_not_reported_as_success(self):
        for variable, message in (
            ("FAIL_GO_BUILD", "simulated Go build failure"),
            ("FAIL_GENERATE", "simulated generation failure"),
        ):
            with self.subTest(variable=variable):
                result = self.run_updater(**{variable: "1"})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertNotIn("update completed!", result.stdout)
                if variable == "FAIL_GO_BUILD":
                    self.assertNotIn(
                        "make manifests generate",
                        (self.root / "go-commands.log").read_text(),
                    )


if __name__ == "__main__":
    unittest.main()
