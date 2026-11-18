#!/usr/bin/env bash
set -euo pipefail

java -version
properties="$(java -XshowSettings:properties -version 2>&1)"
grep -Eq '^[[:space:]]*java.version = 21\.0\.13\.1$' <<<"$properties"
grep -Eq '^[[:space:]]*java.runtime.version = 21\.0\.13\.1\+4([-[:space:]]|$)' <<<"$properties"
grep -Eq '^[[:space:]]*java.vendor = Oracle Corporation$' <<<"$properties"
zkServer.sh version 2>/dev/null | tee /dev/stderr | grep -q '3\.8\.4'

set +e
zu_output="$(java -jar /opt/libs/zu.jar 2>&1)"
zu_status=$?
set -e
printf '%s\n' "$zu_output"
test "$zu_status" -eq 1
grep -q '^Usage: zu ' <<<"$zu_output"

for script in zookeeperStart.sh zookeeperReady.sh zookeeperLive.sh zookeeperTeardown.sh zookeeperMetrics.sh; do
    test -x "/usr/local/bin/$script"
done
command -v dig curl socat pgrep nc >/dev/null

cat > "$ZOOCFGDIR/zoo.cfg" <<EOF
tickTime=2000
dataDir=$ZOO_DATA_DIR
clientPort=2181
initLimit=5
syncLimit=2
standaloneEnabled=false
reconfigEnabled=true
4lw.commands.whitelist=ruok
server.1=127.0.0.1:2888:3888;2181
EOF
printf '1\n' > "$ZOO_DATA_DIR/myid"
zkServer.sh start
trap 'zkServer.sh stop' EXIT
ready=false
for attempt in $(seq 1 60); do
    if [[ "$(printf ruok | nc -w 1 127.0.0.1 2181)" == imok ]]; then
        ready=true
        break
    fi
    sleep 1
done
if [[ "$ready" != true ]]; then
    echo "ZooKeeper did not become ready" >&2
    exit 1
fi
zk_pid="$(pgrep -f 'org.apache.zookeeper.server.quorum.QuorumPeerMain')"
timeout 30 jcmd "$zk_pid" VM.version | tee /dev/stderr | grep -Eq '21\.0\.13\.1\+4'
timeout 30 jcmd "$zk_pid" VM.system_properties | grep -Fx "java.home=$JAVA_HOME"
timeout 30 java -jar /opt/libs/zu.jar get-all 127.0.0.1:2181
