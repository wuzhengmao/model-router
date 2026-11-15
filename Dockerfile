# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build

WORKDIR /src

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=""

RUN apk add --no-cache tzdata

COPY go.mod ./
COPY . .

RUN set -eux; \
    if [ -z "${VERSION}" ]; then \
      VERSION="$(tr -d '[:space:]' < internal/version/version.txt)"; \
    fi; \
    goarch="${TARGETARCH:-$(go env GOARCH)}"; \
    goos="${TARGETOS:-linux}"; \
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" go build \
      -trimpath \
      -ldflags "-s -w -X model-router/internal/version.Version=${VERSION}" \
      -o /model-router .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /model-router /model-router

# Container logs must carry Beijing time (§17.7); distroless ships no tzdata.
COPY --from=build /usr/share/zoneinfo/Asia/Shanghai /usr/share/zoneinfo/Asia/Shanghai
COPY --from=build /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
ENV TZ=Asia/Shanghai

USER nonroot:nonroot
EXPOSE 18080
ENV MODEL_ROUTER_LISTEN=0.0.0.0:18080
ENTRYPOINT ["/model-router", "-config", "/home/nonroot/config/config.json"]
