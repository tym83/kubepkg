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
    go build -trimpath -ldflags "-s -w -X github.com/tym83/kubepkg/pkg/version.Version=${VERSION}" \
    -o /out/kubepkg-operator ./cmd/kubepkg-operator

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kubepkg-operator /kubepkg-operator
USER 65532:65532
ENTRYPOINT ["/kubepkg-operator"]
