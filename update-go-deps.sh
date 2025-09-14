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
            
            # Update each module
            for module in $modules; do
                if [ -n "$module" ]; then
                    go get -u "$module"
                fi
            done
            
            go mod tidy
        )
    else
        echo "Directory $dir does not exist, skipping..."
    fi
done

echo "Go dependencies update completed!"
