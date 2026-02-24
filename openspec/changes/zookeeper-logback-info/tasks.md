## 1. Logback config generation

- [x] 1.1 Add `makeZkLogbackConfigString()` in `pkg/zk/generators.go` returning valid Logback XML with root and CONSOLE appender at INFO, explicit `<logger>` for `org.apache.zookeeper` and `org.eclipse.jetty` at INFO, and pattern `%d{ISO8601} [myid:%X{myid}] - %-5p [%t:%C{1}@%L] - %m%n`
- [x] 1.2 Add `makeZkLogbackQuietConfigString()` in `pkg/zk/generators.go` returning valid Logback XML with root and CONSOLE appender at ERROR and explicit loggers for `org.apache.zookeeper` and `org.eclipse.jetty` at ERROR, same pattern

## 2. ConfigMap integration

- [x] 2.1 In `MakeConfigMap()`, add `"logback.xml": makeZkLogbackConfigString()` and `"logback-quiet.xml": makeZkLogbackQuietConfigString()` to the ConfigMap `Data` map alongside existing keys (`zoo.cfg`, `log4j.properties`, `log4j-quiet.properties`, `env.sh`)

## 3. Verification

- [x] 3.1 Confirm generated ConfigMap contains both `logback.xml` and `logback-quiet.xml` (e.g. unit test or manual check after deploy)

## 4. Startup script

- [x] 4.1 In the ZooKeeper startup script (e.g. `docker/bin/zookeeperStart.sh`), add copy of `logback.xml` and `logback-quiet.xml` from `/conf` to `$ZOOCFGDIR` (e.g. `/data/conf`) alongside the existing copies of `log4j.properties`, `log4j-quiet.properties`, and `env.sh`, so that Logback finds the config when the JVM starts

## 5. E2E test

- [x] 5.1 Add e2eutil helpers in `pkg/test/e2e/e2eutil/pod_util.go`: `GetPodLogs` (stream pod logs with optional `PodLogOptions`) and `PodExec` (run a command in a pod container and return stdout/stderr)
- [x] 5.2 Add e2e test in `test/e2e/logback_config_test.go` that creates a Zookeeper cluster, waits for ready, then (a) execs `ls /data/conf` in a running pod and asserts output contains `logback.xml` and `logback-quiet.xml`, and (b) fetches recent pod logs and asserts at least one INFO line from `org.apache.zookeeper` or `org.eclipse.jetty`, then deletes the cluster
