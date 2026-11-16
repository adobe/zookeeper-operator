#!/bin/bash

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

for command in go gh docker python3 make curl; do
    command -v "$command" >/dev/null || {
        echo "Required command not found: $command" >&2
        exit 1
    }
done

export GOTOOLCHAIN=auto

# Resolve and validate every build pin before writing any build configuration.
python3 - <<'PY'
import json
from pathlib import Path
import re
import subprocess
import sys


def output(*command):
    return subprocess.check_output(command, text=True).strip()


def stable_version(version):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", version):
        sys.exit(f"Expected a stable release version, got {version!r}")
    return version


def latest_release(repo):
    release = json.loads(output("gh", "api", "--hostname", "github.com",
                                f"repos/{repo}/releases/latest"))
    if release.get("draft") or release.get("prerelease"):
        sys.exit(f"Expected a stable release for {repo}")
    return stable_version(release.get("tag_name", ""))


def releases(repo):
    pages = json.loads(output("gh", "api", "--paginate", "--slurp",
                              f"repos/{repo}/releases?per_page=100"))
    return [release for page in pages for release in page
            if not release.get("draft") and not release.get("prerelease")]


def version_tuple(version):
    return tuple(map(int, version.removeprefix("v").split(".")))


files = {}


def update_pin(filename, pattern, value):
    text = files.get(filename)
    if text is None:
        text = Path(filename).read_text()
    text, count = re.subn(pattern, lambda match: match[1] + value,
                          text, flags=re.MULTILINE)
    if count != 1:
        sys.exit(f"Expected one build pin in {filename} for {pattern!r}, found {count}")
    files[filename] = text


go_release = output("curl", "--fail", "--silent", "--show-error",
                    "https://go.dev/VERSION?m=text").partition("\n")[0]
match = re.fullmatch(r"go([0-9]+\.[0-9]+\.[0-9]+)", go_release)
if not match:
    sys.exit(f"Expected a stable Go release, got {go_release!r}")
go_version = match[1]
current_go = re.findall(r"^go ([0-9]+\.[0-9]+(?:\.[0-9]+)?)$",
                        Path("go.mod").read_text(), re.MULTILINE)
if len(current_go) != 1:
    sys.exit("Expected one Go version directive in go.mod")
if tuple(map(int, go_version.split("."))) < tuple(map(int, current_go[0].split("."))):
    sys.exit(f"Refusing to downgrade Go from {current_go[0]} to {go_version}")
update_pin("go.mod", r"^(go )[0-9.]+$", go_version)
update_pin("Dockerfile", r"^(ARG GO_VERSION=)[0-9.]+$",
           ".".join(go_version.split(".")[:2]))
print(f"Go: {go_version}", flush=True)

for variable, module in (
    ("KUSTOMIZE_VERSION", "sigs.k8s.io/kustomize/kustomize/v5"),
    ("CONTROLLER_TOOLS_VERSION", "sigs.k8s.io/controller-tools"),
):
    version = stable_version(output("go", "list", "-m", "-f", "{{.Version}}",
                                    f"{module}@latest"))
    update_pin("Makefile", rf"^({variable} \?= )v[0-9.]+$", version)
    print(f"{variable}: {version}", flush=True)

kind_version = latest_release("kubernetes-sigs/kind")
update_pin("Makefile", r"^(KIND_VERSION \?= )v[0-9.]+$", kind_version)
print(f"KIND_VERSION: {kind_version}", flush=True)

kind_images = []
for release in releases("kubernetes-sigs/kind"):
    kind_images.extend(re.findall(
        r"kindest/node:v([0-9]+\.[0-9]+\.[0-9]+)@(sha256:[0-9a-f]{64})",
        release.get("body") or ""))
node_pins = re.findall(r"node_image: kindest/node:v([0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}",
                      Path(".github/workflows/e2e.yaml").read_text())
if not node_pins:
    sys.exit("Expected pinned kind matrix node images")
for current in node_pins:
    minor = ".".join(current.split(".")[:2])
    candidates = [(version, digest) for version, digest in kind_images
                  if version.startswith(minor + ".") and version_tuple(version) >= version_tuple(current)]
    if not candidates:
        sys.exit(f"No published kind node image for Kubernetes {minor} at least {current}")
    version, digest = max(candidates, key=lambda item: version_tuple(item[0]))
    actual = output("docker", "buildx", "imagetools", "inspect", "--format",
                    "{{.Manifest.Digest}}", f"kindest/node:v{version}")
    if actual != digest:
        sys.exit(f"kind node digest does not match the published release: v{version}")
    update_pin(".github/workflows/e2e.yaml",
               rf"(node_image: kindest/node:v){re.escape(minor)}\.[0-9]+@sha256:[0-9a-f]{{64}}",
               f"{version}@{digest}")
    print(f"kind Kubernetes {minor}: v{version}@{digest}", flush=True)

envtest_tool = output("go", "list", "-m", "-f", "{{.Version}}",
                      "sigs.k8s.io/controller-runtime/tools/setup-envtest@latest")
if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9]{14}-[0-9a-f]{12})?", envtest_tool):
    sys.exit(f"Invalid setup-envtest module version: {envtest_tool!r}")
update_pin("Makefile", r"^(ENVTEST_TOOL_VERSION = )\S+$", envtest_tool)

current_envtest = re.findall(r"^ENVTEST_K8S_VERSION = ([0-9]+\.[0-9]+\.[0-9]+)$",
                            Path("Makefile").read_text(), re.MULTILINE)
if len(current_envtest) != 1:
    sys.exit("Expected one ENVTEST_K8S_VERSION pin")
current_envtest = current_envtest[0]
envtest_minor = ".".join(current_envtest.split(".")[:2])
envtest_versions = []
for release in releases("kubernetes-sigs/controller-tools"):
    match = re.fullmatch(r"envtest-v([0-9]+\.[0-9]+\.[0-9]+)", release.get("tag_name", ""))
    if not match:
        continue
    version = match[1]
    required = {f"envtest-v{version}-{platform}.tar.gz" for platform in ("linux-amd64", "darwin-arm64")}
    assets = {asset["name"] for asset in release.get("assets", [])}
    if version.startswith(envtest_minor + ".") and version_tuple(version) >= version_tuple(current_envtest) and required <= assets:
        envtest_versions.append(version)
if not envtest_versions:
    sys.exit(f"No published envtest assets for Kubernetes {envtest_minor} at least {current_envtest}")
envtest_version = max(envtest_versions, key=version_tuple)
update_pin("Makefile", r"^(ENVTEST_K8S_VERSION = )[0-9.]+$", envtest_version)
print(f"envtest: {envtest_tool}, Kubernetes {envtest_version}", flush=True)

for repo, action, filenames in (
    ("golangci/golangci-lint", "golangci/golangci-lint-action",
     (".github/workflows/ci.yaml",)),
    ("helm/helm", "azure/setup-helm",
     (".github/workflows/ci.yaml", ".github/workflows/release.yml")),
):
    version = latest_release(repo)
    for filename in filenames:
        update_pin(filename,
                   rf"(uses: {re.escape(action)}@\S+\n[ \t]+with:\n[ \t]+version: )v[0-9.]+$",
                   version)
    print(f"{repo}: {version}", flush=True)

for repo, image, filename, strip_v in (
    ("yannh/kubeconform", "ghcr.io/yannh/kubeconform",
     ".github/workflows/ci.yaml", False),
    ("rhysd/actionlint", "rhysd/actionlint",
     ".github/workflows/workflow-lint.yaml", True),
):
    version = latest_release(repo)
    tag = version.removeprefix("v") if strip_v else version
    digest = output("docker", "buildx", "imagetools", "inspect", "--format",
                    "{{.Manifest.Digest}}", f"{image}:{tag}")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        sys.exit(f"Invalid image digest for {image}:{tag}: {digest!r}")
    update_pin(filename, rf"({re.escape(image)}:)[v0-9.]+@sha256:[0-9a-f]{{64}}",
               f"{tag}@{digest}")
    print(f"{image}: {tag}@{digest}", flush=True)

for filename, text in files.items():
    Path(filename).write_text(text)
PY

# Update Go modules dependencies
# Only include directories that actually have go.mod files
DIRS=(".")

for dir in "${DIRS[@]}"; do
    if [ -d "$dir" ]; then
        echo "Updating $dir deps"
        (
            cd "$dir"
            go mod tidy
            
            # Get all non-replaced, non-indirect, non-main modules
            modules=$(go list -mod=readonly -m -f '{{ if and (not .Replace) (not .Indirect) (not .Main)}}{{.Path}}{{end}}' all)
            
            # Update each module to its latest release.
            #
            # Deliberately NOT "go get -u": -u also upgrades every transitive
            # dependency to its own latest version. For modules published
            # without semver tags, "latest" means the latest master commit, so
            # -u drags k8s.io/kube-openapi ahead of the k8s.io/* release that
            # the rest of the stack is pinned to. The resulting mix of
            # sigs.k8s.io/structured-merge-diff v6 (used by apimachinery) and
            # v7 (used by the newer kube-openapi) does not compile.
            #
            # Plain "go get <module>@latest" upgrades the named module only and
            # lets minimal version selection resolve its dependencies to what
            # that release actually requires.
            for module in $modules; do
                if [ -n "$module" ]; then
                    go get "$module@latest"
                fi
            done

            go mod tidy

            # Fail here rather than in CI if the upgrade produced an
            # inconsistent module graph.
            go build ./...
        )
    else
        echo "Directory $dir does not exist, skipping..."
    fi
done

make manifests generate

echo "Go dependencies and build tools update completed!"
