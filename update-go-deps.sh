#!/bin/bash

set -e

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

echo "Go dependencies update completed!"
