# syntax=docker/dockerfile:1
#
# Build from the repository root:
#   docker build -f deploy/Dockerfile -t garmin-mcp .
ARG GO_VERSION=1.27

FROM golang:${GO_VERSION} AS build
WORKDIR /src
# The library is its own module; the workspace (go.work) builds the server
# against the copy in this repository, so its go.mod must be present before the
# download.
COPY go.work go.work.sum go.mod go.sum ./
COPY garmin-connect/go.mod ./garmin-connect/
RUN go mod download
# Only the source directories are copied, so a stray `key` file in the root cannot
# end up in the image; .dockerignore keeps captured data (testdata/live) out.
COPY garmin-connect ./garmin-connect
COPY cmd ./cmd
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/garmin-mcp ./cmd/garmin-mcp
# The key store and audit log live here; private to the unprivileged user the
# image runs as.
RUN mkdir -m 700 /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/garmin-mcp /garmin-mcp
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
EXPOSE 8808
# The start period covers checking a key from the environment with Garmin
# (up to 20 s) before the server starts listening.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/garmin-mcp", "-healthcheck", "-addr", ":8808"]
ENTRYPOINT ["/garmin-mcp"]
CMD ["-addr", ":8808", "-key-store", "/data/garmin.key"]
