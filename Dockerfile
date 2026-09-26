# syntax=docker/dockerfile:1

# Build natively on the runner and cross-compile: the SQLite driver is pure Go, so no emulation or C toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/omega .
# Country database (DB-IP Lite, CC BY 4.0), fetched with the app's own updater. The pipeline rebuilds monthly.
RUN OMEGA_DB=/tmp/geoip/omega.db go run . geoip-update && \
    mkdir -p /out/geoip /out/data && mv /tmp/geoip/dbip-country-lite.mmdb /out/geoip/

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/omega /omega
COPY --from=build /out/geoip/dbip-country-lite.mmdb /usr/share/omega/dbip-country-lite.mmdb
COPY --from=build --chown=65532:65532 /out/data /data
ENV OMEGA_DB=/data/omega.db \
    OMEGA_GEOIP_DB=/usr/share/omega/dbip-country-lite.mmdb \
    PORT=3300
VOLUME /data
EXPOSE 3300
USER nonroot:nonroot
ENTRYPOINT ["/omega"]
