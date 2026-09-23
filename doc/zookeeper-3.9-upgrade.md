# Migrating from ZooKeeper 3.8.4 to 3.9.6

The operator now ships Apache ZooKeeper 3.9.6 as its default server version.

## Why 3.9.6

[ZOOKEEPER-4712](https://issues.apache.org/jira/browse/ZOOKEEPER-4712) is listed by
Apache as fixed in 3.9.3 and 3.10.0 only. It is not fixed in any 3.8.x release, so
moving to the 3.9.x line is the only way to pick it up without going to 3.10.
3.9.6 is the current stable 3.9.x release.

## Where the version is pinned

The Apache ZooKeeper version is managed manually (Dependabot ignores `*zookeeper*`)
and must stay in sync across:

| Location | Meaning |
| --- | --- |
| `ARG ZK_VERSION` in `docker/zookeeper-image/Dockerfile` | Single source of truth. The Apache tarball that is downloaded and PGP-verified. |
| `FROM adobe/zookeeper:<ZK_VERSION>-apache` in `docker/Dockerfile` | Base image for the operator-managed ZooKeeper image. `release.yml` fails the build if the tag does not start with `<ZK_VERSION>-apache`. Bumped by Dependabot. |
| `DefaultZkContainerVersion` in `api/v1beta1/zookeepercluster_types.go` | Default `spec.image.tag` applied to a `ZookeeperCluster` that does not pin one. |
| `org.apache.zookeeper:zookeeper` in `docker/zu/build.gradle.kts` | ZooKeeper client library used by the `zu` helper that performs dynamic reconfiguration. |

### Release order

The pins cannot all be bumped in a single commit: some of them reference image tags
that do not exist until a release has actually published them. The bump is staged.

1. **Bump `ARG ZK_VERSION` (and the `zu` client library) only** and merge. The
   `FROM` line in `docker/Dockerfile` is deliberately left alone, because the base
   image tag it would have to point at does not exist yet. Until step 3 lands, the
   `build-zookeeper` consistency check fails by design, so an operator release tag
   must not be pushed in between.
2. **Push a `zk<date>` git tag.** `build-zookeeper-apache` is the only job that runs
   for `zk*` tags. It builds `docker/zookeeper-image` and pushes two tags to Docker
   Hub and GHCR: the immutable `3.9.6-apache-zk<date>` and the floating
   `3.9.6-apache`.
3. **Merge the Dependabot PR** that bumps `docker/Dockerfile` to
   `adobe/zookeeper:3.9.6-apache`. This is why the `FROM` line uses the floating tag:
   Dependabot only offers a Docker update when the tag suffix is unchanged, so a
   dated suffix would never be bumped automatically. At this point the two files are
   consistent again.
4. **Push the operator release tag**, which builds the ZooKeeper and operator images
   and publishes the Helm chart.
5. **Update `DefaultZkContainerVersion`** to `3.9.6-<tag>` from step 4, so new
   clusters that do not pin `spec.image.tag` default to an image that exists.

## Configuration considerations

No configuration changes are required. The `zoo.cfg` the operator generates
(`4lw.commands.whitelist`, `metricsProvider.className`, `admin.serverPort`,
dynamic reconfiguration via `reconfigEnabled`) is valid unchanged on 3.9.x.

* **Java** — 3.9.x requires Java 11 or newer. The image is built on OpenJDK 21, so
  this is already satisfied.
* **Clients** — 3.9.x servers remain wire-compatible with 3.8.x and older clients.
  Applications do not need to be upgraded at the same time. Users of Apache Curator
  should be on Curator 5.x.
* **Data format** — the snapshot and transaction log formats are unchanged between
  3.8.x and 3.9.x, so existing persistent volumes are read as-is.

## Upgrade procedure

Apache supports a rolling upgrade from 3.8.x to 3.9.x, and the operator performs one
pod at a time while waiting for each pod to rejoin the quorum. Quorum is preserved
for clusters of three or more replicas.

1. **Take a backup.** Snapshot the `data` PVCs (or copy `version-2/`) before
   starting. This is what makes step 5 possible.
2. **Upgrade the operator first** via `helm upgrade`, so the controller knows the new
   `DefaultZkContainerVersion`.
3. **Move the cluster to the new image.** Clusters that do not set
   `spec.image.tag` pick up the new default on the next reconcile. Clusters that pin
   a tag need an explicit update to the image published by step 4 above:

   ```yaml
   spec:
     image:
       repository: ghcr.io/adobe/zookeeper-operator/zookeeper
       tag: 3.9.6-<operator release tag>
   ```

4. **Watch the rollout.**

   ```bash
   kubectl get zookeepercluster <name> -o jsonpath='{.status.currentVersion} -> {.status.targetVersion}{"\n"}'
   kubectl get pods -l app=<name> -w
   ```

   `status.conditions` reports `Upgrading`, and `UpgradeFailed` if a pod does not
   become ready within the configured timeout. Confirm each server with
   `echo srvr | nc <pod> 2181` and check that exactly one reports `Mode: leader`.

5. **Rollback.** Set `spec.image.tag` back to the 3.8.x tag. Because the on-disk
   format is unchanged, a rollback that happens before any 3.9-only feature is
   enabled is safe; restore from the step 1 backup otherwise.

## Recommended path for existing 3.8.4 users

Upgrade 3.8.4 to the latest 3.8.x first, then to 3.9.6, rather than jumping directly.
Each hop is a rolling upgrade using the procedure above.
