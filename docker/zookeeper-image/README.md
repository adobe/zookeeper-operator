# Apache ZooKeeper base image

Build from the repository root:

```bash
docker buildx build \
  --build-arg ZK_VERSION="$(./scripts/zookeeper-version.sh)" \
  -t zookeeper-apache:local docker/zookeeper-image
```

The image uses Debian Trixie and Java 25. Oracle's public JDK download URL is
the default, without authentication.

## Alternate JDK downloads

Override `java_download_base_url` and `java_download_version` to use another
source. The Dockerfile appends `java_version` (default: `25`) and `latest`
for Oracle, or `java_version` and `java_download_version` for alternate
sources. Neither version belongs in the base URL. Alternate sources must
provide Oracle-style archive filenames and matching `.sha256` files for
each architecture (`x64` and `aarch64`).

For example, add these options to the build command:

```bash
--build-arg java_download_base_url=https://jdk.example.com/java \
--build-arg java_download_version=25.0.1.1
```

This requests `/java/25/25.0.1.1/jdk-25.0.1.1_linux-<arch>_bin.tar.gz`
and its `.sha256` file. Both downloads must succeed and the checksum must match.

For authenticated downloads, pass an optional API key as a BuildKit secret:

```bash
--secret id=JAVA_DOWNLOAD_APIKEY,env=JAVA_DOWNLOAD_APIKEY
```

The key is sent as an `Authorization: Bearer` header, is not stored in the
image, and is not forwarded to a different host on redirects.

Release builds also default to Oracle. To use an alternate source, add the
commented overrides in `.github/workflows/release.yml` to the Apache ZooKeeper
build step's `build-args`, configure the GitHub Actions secret
`JAVA_DOWNLOAD_BASE_URL`, and select a matching `java_download_version`.
For authenticated downloads, also enable the commented `secrets` input and
configure `JAVA_DOWNLOAD_APIKEY`. PR image builds continue to use Oracle
without secrets.
