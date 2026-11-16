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

# §24.10 B2: seed the config directory so a fresh named volume inherits
# nonroot ownership and a usable placeholder config.json.
RUN mkdir -p /out/config \
    && cp /src/config.example.json /out/config/config.json \
    && touch /out/config/.keep

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /model-router /model-router

# Config and stats.json share this directory; declaring it a VOLUME keeps
# container data in an anonymous/named volume when no bind mount is supplied.
COPY --from=build --chown=65532:65532 /out/config/ /home/nonroot/config/
VOLUME ["/home/nonroot/config"]

# Container logs must carry Beijing time (§17.7); distroless ships no tzdata.
COPY --from=build /usr/share/zoneinfo/Asia/Shanghai /usr/share/zoneinfo/Asia/Shanghai
COPY --from=build /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
ENV TZ=Asia/Shanghai

USER nonroot:nonroot
EXPOSE 18080
ENV MODEL_ROUTER_LISTEN=0.0.0.0:18080
ENTRYPOINT ["/model-router", "-config", "/home/nonroot/config/config.json"]
