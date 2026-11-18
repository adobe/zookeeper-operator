# Apache ZooKeeper base image

Build from the repository root:

```bash
docker buildx build --load \
  --build-arg ZK_VERSION="$(./scripts/zookeeper-version.sh)" \
  -t zookeeper-apache:local docker/zookeeper-image
```

The image uses Debian Trixie and this hotfix defaults to Java 21. Oracle's public JDK
download URL is the default, without authentication.

## Alternate JDK downloads

Override `java_download_base_url` and `java_download_version` to use another
source. The Dockerfile appends `java_version` (default: `21`) and `latest`
for Oracle, or only `java_version` for alternate sources. Do not include
the Java-major directory in the base URL. Alternate sources must
provide Oracle-style archive filenames and matching `.sha256` files for
each architecture (`x64` and `aarch64`).

For example, add these options to the build command:

```bash
--build-arg java_download_base_url=https://jdk.example.com/java \
--build-arg java_version=21 \
--build-arg java_download_version=21.0.13.1
```

This requests `/java/21/jdk-21.0.13.1_linux-<arch>_bin.tar.gz`
and its `.sha256` file. Both downloads must succeed and the checksum must match.

For authenticated downloads, pass an optional API key as a BuildKit secret:

```bash
--secret id=JAVA_DOWNLOAD_APIKEY,env=JAVA_DOWNLOAD_APIKEY
```

The key is sent as an `Authorization: Bearer` header, is not stored in the
image, and is not forwarded to a different host on redirects.

This hotfix enables the authenticated override in `.github/workflows/release.yml`
and selects Oracle JDK `21.0.13.1`. Configure repository secrets
`JAVA_DOWNLOAD_BASE_URL` and `JAVA_DOWNLOAD_APIKEY` before publishing. The base
URL must be the approved raw Image Factory repository, ending in
`generic-ifup-resources-release/java/oracle`, without a version directory.
The credential is passed only as a BuildKit secret.

## Full ZooKeeper image

After building the base image above, build the operator-flavoured image:

```bash
docker buildx build --load \
  --build-arg BASE_IMAGE=zookeeper-apache:local \
  -t zookeeper:local docker
```

The full image reads the base image's JDK release metadata and uses its
Java major version for both Java and Kotlin compilation targets in `zu`.
Set `java_version` only when building the base image; no second version
argument is needed for the full image.

The native Gradle builder uses JDK 25 independently of the selected runtime.
Direct Gradle builds, including `./gradlew build --no-daemon`, read the
default from the same base Dockerfile. An explicit `-PjavaVersion=<major>`
overrides that target when needed.

## Production hotfix

The branch starts at production tag `0.2.15-adobe-20260225`. The four commits
from [PR #113](https://github.com/adobe/zookeeper-operator/pull/113) are retained
as separate cherry-picks with their original file names. Emergency overrides
and old-release compatibility changes are a separate commit.

ZooKeeper remains `3.8.4`; the existing `zu` client dependency remains `3.9.3`.
The shared server version resolver and helper shell function are prerequisites
used by the unchanged upstream tests. The native builder retains JDK 25 from
upstream, with its matching Gradle/Kotlin/Shadow toolchain; it does not ship in
the runtime image. The compilation target follows the selected Java 21 base.

Oracle identifies `21.0.13.1+4` as the fixed JDK 21 build for
[CVE-2026-114938](https://www.oracle.com/security-alerts/cspunov2026.html#AppendixJAVA);
see the [JDK release notes](https://www.oracle.com/java/technologies/javase/21all-relnotes.html#R21_0_13_1).
Confirm runner access and Oracle redistribution approval before publication.

Run `python3 -B -m unittest discover -s scripts -p 'test_*.py'` for all upstream
build-script tests. Run `zookeeper-images` manually on the reviewed hotfix ref
to build both architectures with the same approved JDK override, without
publication. The additional smoke test verifies the exact Oracle build in the
running ZooKeeper process, server readiness and live `zu` access.

This old release line publishes on **tag push**, not on GitHub Release
publication. Use a new Helm-compatible tag on the reviewed hotfix commit;
never move an existing tag. A regular tag (not starting with `zk`) rebuilds
the Apache base and pins the wrapper to its digest. It also publishes the
operator image and Helm chart, which need not be deployed for this runtime fix.

The final server image is `adobe/zookeeper:3.8.4-<release-tag>`.
Verify the published digest and JVM before updating `pipeline-config`.
Publishing does not roll out the image to existing clusters.
