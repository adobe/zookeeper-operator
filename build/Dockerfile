FROM alpine:3.24

RUN apk upgrade --update --no-cache

USER nobody

ADD build/_output/bin/zookeeper-operator /usr/local/bin/zookeeper-operator
