# Design: Add Logback config so ZooKeeper logs at INFO

## Context

The operator generates a ConfigMap per ZookeeperCluster containing `zoo.cfg`, `env.sh`, `log4j.properties`, and `log4j-quiet.properties`. The ConfigMap is mounted at `/conf` and the pod’s startup script copies its contents to a writable config dir (e.g. `/data/conf`) before running `zkServer.sh --config /data/conf start-foreground`. The ZooKeeper server image uses **Logback** (logback-classic, logback-core) as the SLF4J backend; there is no Log4j on the classpath, so the existing log4j config files are never read. Logback discovers configuration by looking for `logback.xml` (or `logback.groovy` / `logback-test.xml`) on the classpath; `ZOOCFGDIR` is prepended to the classpath in `zkEnv.sh`, so a `logback.xml` placed in the config dir will be picked up when the JVM starts. The goal is to add operator-generated Logback config so that runtime logging is at INFO (or optionally ERROR for a quiet profile) instead of DEBUG.

## Goals / Non-Goals

**Goals:**

- Generate `logback.xml` with root and key loggers at INFO, and `logback-quiet.xml` with root and key loggers at ERROR.
- Include both files in the existing ZookeeperCluster ConfigMap; no new volume or mount.
- Use a CONSOLE appender with the same pattern as upstream ZooKeeper (`%d{ISO8601} [myid:%X{myid}] - %-5p [%t:%C{1}@%L] - %m%n`) for consistency.
- Explicitly set logger level for `org.apache.zookeeper` and `org.eclipse.jetty` so DEBUG from those packages is suppressed regardless of any default or bundled config.

**Non-Goals:**

- No CRD field or API to select log level (INFO vs ERROR); both configs are present and the default is `logback.xml` (INFO) via Logback’s default discovery. Optional future: env or CRD to point to the quiet file via `-Dlogback.configurationFile`.
- No removal of existing log4j config files (kept for any image that might use Log4j). The **startup script is updated** so that Logback configs are copied and take effect (see Decision 5).

## Decisions

### 1. Generate Logback XML in Go as raw strings

**Decision:** Implement two functions (e.g. `makeZkLogbackConfigString()`, `makeZkLogbackQuietConfigString()`) that return the full XML as Go raw string literals.

**Rationale:** The content is static and small; no user or cluster-specific substitution is required. Using a raw string avoids escaping and keeps the XML readable in code. Alternatives (embedding a file, or using text/template with a tiny template) add indirection without benefit for this size.

### 2. Two separate files (logback.xml and logback-quiet.xml)

**Decision:** Emit two files: `logback.xml` (INFO) and `logback-quiet.xml` (ERROR). Logback’s default lookup uses `logback.xml`, so the default behavior is INFO without any startup-script or env change.

**Rationale:** Matches the existing pattern (log4j.properties vs logback-quiet.properties). A single parameterized file would require the image to pass `-Dlogback.configurationFile` or similar, which is out of scope. Keeping the default as INFO satisfies the main use case (reduce DEBUG); the quiet file is available for future use (e.g. script or env pointing to it).

### 3. Keep existing log4j files unchanged

**Decision:** Do not remove or alter `log4j.properties` or `log4j-quiet.properties` in the ConfigMap.

**Rationale:** Some images or environments might still use Log4j; removing those files could break them. The change is purely additive.

### 4. CONSOLE appender only; explicit loggers for ZooKeeper and Jetty

**Decision:** Each Logback config has one CONSOLE appender with a ThresholdFilter at the chosen level, plus `<logger name="org.apache.zookeeper" level="…"/>` and `<logger name="org.eclipse.jetty" level="…"/>`, and `<root level="…">` with that appender.

**Rationale:** CONSOLE-only matches current operator behavior (no file appenders). Explicit loggers ensure that even if another config or a jar’s default sets DEBUG for those packages, our config takes precedence when our file is loaded first on the classpath.

### 5. Startup script copies Logback configs to writable config dir

**Decision:** The ZooKeeper startup script (e.g. `zookeeperStart.sh`) SHALL copy `logback.xml` and `logback-quiet.xml` from the mounted config source (e.g. `/conf`) to the writable config directory (e.g. `/data/conf`, i.e. `ZOOCFGDIR`) before invoking `zkServer.sh --config $ZOOCFGDIR start-foreground`, in the same way it already copies `log4j.properties`, `log4j-quiet.properties`, and `env.sh`.

**Rationale:** The ConfigMap is mounted read-only at `/conf`. The server runs with `ZOOCFGDIR=/data/conf` on the classpath; that directory is writable and is populated by the script. If the script does not copy the Logback files, they never appear in `/data/conf`, so Logback does not find them and falls back to default (DEBUG). Copying them ensures they are present when the JVM starts.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Another `logback.xml` (e.g. inside a JAR) is found before the config dir on the classpath | ZOOCFGDIR is prepended in zkEnv.sh; as long as the pod uses that script and the same config dir, our file is first. Explicit logger levels still help if both configs are merged in some scenarios. |
| Startup script does not copy Logback files to writable config dir | The startup script in the repo (`docker/bin/zookeeperStart.sh`) is updated to copy `logback.xml` and `logback-quiet.xml` from `/conf` to `$ZOOCFGDIR` alongside the existing log4j and env.sh copies. Rebuilding the image that includes this script is required for the fix to take effect. |
| Need to switch to ERROR at runtime without redeploy | Not supported in this change; would require env/CRD and `-Dlogback.configurationFile` in the image. Document as a possible future improvement. |

## Migration Plan

- **Deployment:** No migration steps for the operator. New ConfigMap entries are additive; existing keys and mounts unchanged. The **image** that runs the ZooKeeper container must include the updated startup script (copying logback.xml and logback-quiet.xml); rebuild and push the image, then roll out the new image to the StatefulSet.
- **Rollout:** On next rollout with the updated image, pods will copy `logback.xml` and `logback-quiet.xml` to the config dir and the JVM will load `logback.xml` from that dir. No data or API migration.
- **Rollback:** Revert the operator change and the startup script change; redeploy the previous image. ConfigMap will no longer contain the Logback keys; script will no longer copy them. Logback will fall back to default or another config on the classpath (e.g. from a JAR); behavior returns to whatever it was before (e.g. DEBUG).

## Open Questions

- **Optional:** Add a ZookeeperCluster field (e.g. `spec.logging.level: Info | Error`) and set `LOG_BACK_CONFIG_FILE` or JVM `-Dlogback.configurationFile` in env so the image can choose the quiet file without changing the image. Defer until someone needs it.
