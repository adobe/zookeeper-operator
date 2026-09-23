# GO_VERSION must be supplied at build time (see Makefile/CI, which derive it
# from the `go` directive in go.mod) so the builder image always matches the
# toolchain version the module actually requires. The default below is only a
# fallback for ad-hoc `docker build .` invocations without --build-arg.
ARG GO_VERSION=1.26
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:${GO_VERSION}-alpine3.24 AS go-builder

# Populated automatically by BuildKit with the target platform of each build.
ARG TARGETOS
ARG TARGETARCH

ARG PROJECT_NAME=zookeeper-operator
# Must match the module path in go.mod, otherwise the -X ldflags below
# silently do nothing and the binary reports its hardcoded default version.
ARG REPO_PATH=github.com/pravega/$PROJECT_NAME

# Build version and commit should be passed in when performing docker build
ARG VERSION=0.0.0-localdev
ARG GIT_SHA=0000000

WORKDIR /src
COPY pkg ./pkg
COPY cmd ./cmd
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Download all dependencies.
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/

# Build
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} CGO_ENABLED=0 go build -o /src/${PROJECT_NAME} \
    -ldflags "-X ${REPO_PATH}/pkg/version.Version=${VERSION} -X ${REPO_PATH}/pkg/version.GitSHA=${GIT_SHA}" main.go

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS final

# Re-declare ARG variables for the final stage
ARG PROJECT_NAME=zookeeper-operator
ARG VERSION=0.0.0-localdev
ARG GIT_SHA=0000000
ARG BUILT_AT

COPY --from=go-builder /src/${PROJECT_NAME} /usr/local/bin/${PROJECT_NAME}

# OCI standard labels
LABEL org.opencontainers.image.title="Zookeeper Operator"
LABEL org.opencontainers.image.description="Zookeeper Operator for Kubernetes"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${GIT_SHA}"
LABEL org.opencontainers.image.created="${BUILT_AT}"
LABEL org.opencontainers.image.source="https://github.com/adobe/zookeeper-operator"
LABEL org.opencontainers.image.url="https://github.com/adobe/zookeeper-operator"
LABEL org.opencontainers.image.documentation="https://github.com/adobe/zookeeper-operator"
LABEL org.opencontainers.image.vendor="Adobe"
LABEL org.opencontainers.image.licenses="Apache-2.0"

ENTRYPOINT ["/usr/local/bin/zookeeper-operator"]
