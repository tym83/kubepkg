# The kubepkg operator image. BUILDER can point at a Docker Hub mirror.
ARG BUILDER=golang:1.26
FROM --platform=$BUILDPLATFORM ${BUILDER} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/kuberoot-dev/kubepkg/pkg/version.Version=${VERSION}" \
    -o /out/kubepkg-operator ./cmd/kubepkg-operator

# nelm, werf's deployment engine, for the werf backend; pinned by checksum.
FROM --platform=$BUILDPLATFORM ${BUILDER} AS nelm
ARG TARGETARCH
ARG NELM_VERSION=1.31.1
ARG NELM_SHA256_amd64=45178ece53a9b12d1d15b53d138f0d8fedf29362b12dc146015fd85b79707633
ARG NELM_SHA256_arm64=a4525597fad6525411b16f74995c59c16fe14fc49ee10665ddadc92bbfc581ca
RUN curl -fsSL -o /nelm "https://tuf.nelm.sh/targets/releases/${NELM_VERSION}/linux-${TARGETARCH}/bin/nelm" \
    && eval "want=\$NELM_SHA256_${TARGETARCH}" \
    && echo "${want}  /nelm" | sha256sum -c - \
    && chmod 0755 /nelm

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kubepkg-operator /kubepkg-operator
COPY --from=nelm /nelm /usr/local/bin/nelm
USER 65532:65532
ENTRYPOINT ["/kubepkg-operator"]
