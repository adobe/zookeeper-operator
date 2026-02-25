# Proposal: Add Logback config so ZooKeeper logs at INFO

## Why

ZooKeeper pods managed by the operator log at DEBUG level, producing very high log volume from `org.apache.zookeeper` and `org.eclipse.jetty` (CommitProcessor, DataTree, NettyServerCnxn, Jetty lifecycle, etc.). This makes logs hard to use in production and can impact performance and log storage. The operator already generates `log4j.properties` and `log4j-quiet.properties`, but the ZooKeeper server image uses **Logback** as its SLF4J backend (logback-classic, logback-core in `lib/`), not Log4j, so those files are never read. To reduce log level to INFO we must provide a Logback configuration that is actually used at runtime.

## What Changes

- **Operator ConfigMap**: Generate and include `logback.xml` (root and key loggers at INFO) and `logback-quiet.xml` (root and key loggers at ERROR) in the ZookeeperCluster ConfigMap, alongside existing log4j files.
- **Logback content**: Both files use a CONSOLE appender with the same pattern as upstream ZooKeeper (`%d{ISO8601} [myid:%X{myid}] - %-5p ...`). Explicit `<logger>` entries for `org.apache.zookeeper` and `org.eclipse.jetty` at the chosen level so DEBUG from those packages is suppressed regardless of any default config.
- **No removal**: Existing `log4j.properties` and `log4j-quiet.properties` remain for images or setups that use Log4j; no breaking change to the ConfigMap shape or mount paths.
- **Runtime behavior**: When the pod’s config dir (e.g. `/data/conf` or `/conf`) is on the classpath before other resources, Logback will load `logback.xml` from that dir and apply INFO (or ERROR when using the quiet variant), reducing log volume to a manageable level.

## Capabilities

### New Capabilities

- **zookeeper-logback-config**: The operator supplies Logback configuration (logback.xml and an optional quiet variant) so that ZooKeeper servers using Logback at runtime log at INFO by default, with an option for a quieter (ERROR) profile. Covers generation of the XML configs, inclusion in the ConfigMap, and the contract that the chosen file is on the server classpath so Logback picks it up.

### Modified Capabilities

- _(None. No existing specs in `openspec/specs/`; this is a new capability only.)_

## Impact

- **Code**: `pkg/zk/generators.go` — new helpers to generate logback.xml and logback-quiet.xml strings; ConfigMap `Data` extended with `logback.xml` and `logback-quiet.xml`.
- **APIs**: No change to ZookeeperCluster CRD or public API.
- **Dependencies**: None; no new libraries.
- **Systems**: Pods that use the operator’s ConfigMap and a ZooKeeper image with Logback on the classpath will start using the new config on next rollout; config dir must be on the classpath (already true for the current startup flow). Images that use Log4j continue to rely on existing log4j.* properties.
