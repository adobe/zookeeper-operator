# Spec: zookeeper-logback-config

The operator supplies Logback configuration so that ZooKeeper servers using Logback at runtime log at INFO by default, with an optional quieter (ERROR) profile.

## ADDED Requirements

### Requirement: Operator generates default Logback config at INFO

The operator SHALL generate a Logback configuration file named `logback.xml` with root logger and CONSOLE appender at INFO level. The configuration SHALL set explicit logger level to INFO for the packages `org.apache.zookeeper` and `org.eclipse.jetty` so that DEBUG output from those packages is not emitted.

#### Scenario: Default config caps ZooKeeper and Jetty at INFO

- **WHEN** a ZookeeperCluster is reconciled and the ConfigMap is generated
- **THEN** the ConfigMap SHALL contain a key `logback.xml` whose value is valid Logback XML with `<root level="INFO">` and `<logger name="org.apache.zookeeper" level="INFO"/>` and `<logger name="org.eclipse.jetty" level="INFO"/>`

#### Scenario: Default config uses CONSOLE appender with ZooKeeper pattern

- **WHEN** the generated `logback.xml` is inspected
- **THEN** it SHALL contain a CONSOLE appender with a pattern that includes `%d{ISO8601}`, `[myid:%X{myid}]`, and `%-5p` so that log format is consistent with upstream ZooKeeper

### Requirement: Operator generates quiet Logback config at ERROR

The operator SHALL generate a Logback configuration file named `logback-quiet.xml` with root logger and CONSOLE appender at ERROR level. The configuration SHALL set explicit logger level to ERROR for `org.apache.zookeeper` and `org.eclipse.jetty`.

#### Scenario: Quiet config caps all at ERROR

- **WHEN** a ZookeeperCluster is reconciled and the ConfigMap is generated
- **THEN** the ConfigMap SHALL contain a key `logback-quiet.xml` whose value is valid Logback XML with `<root level="ERROR">` and `<logger name="org.apache.zookeeper" level="ERROR"/>` and `<logger name="org.eclipse.jetty" level="ERROR"/>`

### Requirement: Logback configs are in the cluster ConfigMap

The operator SHALL include both `logback.xml` and `logback-quiet.xml` in the same ConfigMap that is used for the ZookeeperCluster (the one mounted at the config directory). No separate volume or mount SHALL be required.

#### Scenario: ConfigMap contains both Logback files

- **WHEN** the ZookeeperCluster ConfigMap is listed
- **THEN** its `data` SHALL include the keys `logback.xml` and `logback-quiet.xml` in addition to existing keys (e.g. `zoo.cfg`, `env.sh`, `log4j.properties`, `log4j-quiet.properties`)

### Requirement: Config directory is on server classpath

For the Logback configuration to take effect, the ZooKeeper server process SHALL have the config directory (the directory where `logback.xml` is placed, e.g. `/data/conf` or `/conf`) on its Java classpath before any JAR or resource that might contain another `logback.xml`. The startup script SHALL copy `logback.xml` and `logback-quiet.xml` from the mounted config source into that directory before starting the server, so that Logback discovers them.

#### Scenario: Logback discovers operator config when config dir is first on classpath

- **WHEN** the ZooKeeper server starts with the operator’s ConfigMap mounted and the config directory is the first element on the classpath
- **THEN** Logback SHALL load `logback.xml` from that directory and the effective log level for `org.apache.zookeeper` and `org.eclipse.jetty` SHALL be INFO (or ERROR if the process is configured to use `logback-quiet.xml`)

### Requirement: Startup script copies Logback configs to writable config directory

The ZooKeeper startup script (e.g. `zookeeperStart.sh`) SHALL copy `logback.xml` and `logback-quiet.xml` from the mounted config source (e.g. `/conf`) to the writable config directory used at runtime (e.g. `/data/conf`, i.e. `ZOOCFGDIR`) before invoking the ZooKeeper server, in the same way it copies `log4j.properties`, `log4j-quiet.properties`, and `env.sh`. This ensures the files are present on the classpath when the JVM starts.

#### Scenario: Script copies Logback files alongside other config files

- **WHEN** the pod starts and the startup script runs before `zkServer.sh --config $ZOOCFGDIR start-foreground`
- **THEN** the script SHALL copy `/conf/logback.xml` and `/conf/logback-quiet.xml` to `$ZOOCFGDIR` (e.g. `/data/conf`), so that both files exist in the same directory as `zoo.cfg`, `log4j.properties`, `log4j-quiet.properties`, and `env.sh`
