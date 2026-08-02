# syntax=docker/dockerfile:1

# Tailwind's output is platform-independent, so this stage is pinned to the
# build machine's platform (--platform=$BUILDPLATFORM) rather than the
# target platform — otherwise buildx would build it twice under QEMU
# emulation for linux/amd64,linux/arm64 for no benefit.
FROM --platform=$BUILDPLATFORM node:22-alpine AS webui-build
WORKDIR /webui
COPY webui-src/package.json webui-src/package-lock.json ./
RUN npm ci
COPY webui-src ./
# tailwind.config.js's content glob is "../internal/webui/assets/**" (relative
# to webui-src, matching the real repo layout) — mirror that layout here so
# the container build scans the same markup as a local `npm run build`.
COPY internal/webui/assets /internal/webui/assets
RUN npm run build

FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY --from=webui-build /webui/dist/styles.css ./internal/webui/assets/styles.css
COPY --from=webui-build /webui/dist/vendor ./internal/webui/assets/vendor

ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]

