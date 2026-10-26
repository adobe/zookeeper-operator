#!/bin/bash

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

for command in go gh docker python3 make; do
    command -v "$command" >/dev/null || {
        echo "Required command not found: $command" >&2
        exit 1
    }
done

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


for variable, module in (
    ("KUSTOMIZE_VERSION", "sigs.k8s.io/kustomize/kustomize/v5"),
    ("CONTROLLER_TOOLS_VERSION", "sigs.k8s.io/controller-tools"),
):
    version = stable_version(output("go", "list", "-m", "-f", "{{.Version}}",
                                    f"{module}@latest"))
    update_pin("Makefile", rf"^({variable} \?= )v[0-9.]+$", version)
    print(f"{variable}: {version}", flush=True)

for repo, action, filenames in (
    ("golangci/golangci-lint", "golangci/golangci-lint-action",
     (".github/workflows/ci.yaml",)),
    ("helm/helm", "azure/setup-helm",
     (".github/workflows/ci.yaml", ".github/workflows/release.yml")),
    ("zizmorcore/zizmor", "zizmorcore/zizmor-action",
     (".github/workflows/workflow-lint.yaml",)),
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
