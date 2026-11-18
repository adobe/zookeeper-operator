# Apache ZooKeeper base image

Build from the repository root:

```bash
docker buildx build --load \
  --build-arg ZK_VERSION="$(./scripts/zookeeper-version.sh)" \
  -t zookeeper-apache:local docker/zookeeper-image
```

The image uses Debian Trixie and defaults to Java 25. Oracle's public JDK
download URL is the default, without authentication.

## Alternate JDK downloads

Override `java_download_base_url` and `java_download_version` to use another
source. The Dockerfile appends `java_version` (default: `25`) and `latest`
for Oracle, or `java_version` and `java_download_version` for alternate sources.
A trailing slash on the base URL is optional. Do not include
the Java-major or patch-version directories in the base URL. Alternate sources must
provide Oracle-style archive filenames and matching `.sha256` files for
each architecture (`x64` and `aarch64`).

For example, add these options to the build command:

```bash
--build-arg java_download_base_url=https://jdk.example.com/java \
--build-arg java_version=21 \
--build-arg java_download_version=21.0.13.1
```

This requests `/java/21/21.0.13.1/jdk-21.0.13.1_linux-<arch>_bin.tar.gz`
and its `.sha256` file. Both downloads must succeed and the checksum must match.

For authenticated downloads, pass an optional API key as a BuildKit secret:

```bash
--secret id=JAVA_DOWNLOAD_APIKEY,env=JAVA_DOWNLOAD_APIKEY
```

The key is sent as an `Authorization: Bearer` header, is not stored in the
image, and is not forwarded to a different host on redirects.

This hotfix's release workflow selects Java `21.0.13.1` with Java major `21`.
Set `JAVA_DOWNLOAD_BASE_URL` to the raw Artifactory repository ending in
`generic-jdk-release/java/oracle` and configure `JAVA_DOWNLOAD_APIKEY` with a
read credential. The workflow passes the credential as the BuildKit secret
shown above. PR image builds continue to use Oracle without secrets.

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
