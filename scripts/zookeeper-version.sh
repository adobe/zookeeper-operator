#!/usr/bin/env bash
set -euo pipefail

version="$(sed -n 's/^val zookeeperVersion = "\([^"]*\)"$/\1/p' "$(dirname "$0")/../docker/zu/build.gradle.kts")"
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Expected one stable zookeeperVersion in docker/zu/build.gradle.kts" >&2
    exit 1
fi
printf '%s\n' "$version"
