# Static build, distroless runtime. Both binaries ship in one image and the
# entrypoint picks which to run, so the API and the indexer can never drift
# apart on the ingestion contract they share.

# Base images default to mirrors rather than Docker Hub. AWS ECR Public carries
# every official Docker Hub image under docker/library/ with identical digests,
# and does not need a Docker Hub login. Override either one at build time:
#   docker compose build --build-arg GO_IMAGE=mirror.gcr.io/library/golang:1.26-alpine
#
# The Go image must satisfy the go directive in go.mod. The official image sets
# GOTOOLCHAIN=local, so it will not download a newer toolchain on its own.
ARG GO_IMAGE=public.ecr.aws/docker/library/golang:1.26-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build

# Module downloads can be pointed at a different proxy the same way, for
# networks where proxy.golang.org is unreachable.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# Dependencies first so a source change does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/earlypair-api ./cmd/earlypair-api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/earlypair-indexer ./cmd/earlypair-indexer

FROM ${RUNTIME_IMAGE}

COPY --from=build /out/earlypair-api /usr/local/bin/earlypair-api
COPY --from=build /out/earlypair-indexer /usr/local/bin/earlypair-indexer
COPY --from=build /src/config /config

USER nonroot:nonroot
WORKDIR /

EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/earlypair-api"]
CMD ["-config", "/config/earlypair.yaml"]
