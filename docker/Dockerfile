# syntax=docker/dockerfile:1
# check=skip=InvalidDefaultArgInFrom
#
# Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#

# Base Apache ZooKeeper + OpenJDK image built from docker/zookeeper-image.
# It is not pinned here: the build-zookeeper release job builds that image
# first and passes its digest in, so both always come from the same commit
# and shared zookeeperVersion. For local builds use `make build-zk-image`.
ARG BASE_IMAGE

FROM ${BASE_IMAGE} AS runtime

# zu.jar is architecture-independent bytecode, so build it once on the native
# build platform instead of re-running Gradle under emulation for every target.
FROM --platform=$BUILDPLATFORM gradle:jdk25 AS zu-builder

RUN mkdir /zu
COPY zu /zu
WORKDIR /zu
RUN --mount=type=bind,from=runtime,source=/usr/local,target=/runtime-java \
    java_version="$(sed -n 's/^JAVA_VERSION="\([0-9][0-9]*\).*/\1/p' /runtime-java/openjdk-*/release)" && \
    test -n "$java_version" && \
    ./gradlew -PjavaVersion="$java_version" shadowJar --no-daemon

FROM runtime
COPY bin /usr/local/bin
RUN chmod +x /usr/local/bin/*
COPY --from=zu-builder /zu/build/libs/zu.jar /opt/libs/

RUN apt-get -q update && \
    apt-get install --no-install-recommends -y dnsutils curl procps socat

# OCI standard labels
LABEL org.opencontainers.image.title="Apache Zookeeper"
LABEL org.opencontainers.image.description="Apache Zookeeper image build with java-${JAVA_VERSION}"
LABEL org.opencontainers.image.source="https://github.com/adobe/zookeeper-operator"
LABEL org.opencontainers.image.url="https://github.com/adobe/zookeeper-operator"
LABEL org.opencontainers.image.documentation="https://github.com/adobe/zookeeper-operator"
LABEL org.opencontainers.image.vendor="Adobe"
LABEL org.opencontainers.image.licenses="Apache-2.0"
