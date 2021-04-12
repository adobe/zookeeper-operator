#
# Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#

FROM azul/zulu-openjdk:11
RUN mkdir /zu
COPY zu /zu
WORKDIR /zu
RUN ./gradlew --console=verbose --info shadowJar

# use forked base zookeeper 3.6.3 docker image
# that runs on azul openjdk-16
FROM adobe/zookeeper:3.6.3-apache-zk-20210413
COPY bin /usr/local/bin
RUN chmod +x /usr/local/bin/*
COPY --from=0 /zu/build/libs/zu.jar /root/

RUN apt-get -q update && \
    apt-get install --no-install-recommends -y curl dnsutils procps
