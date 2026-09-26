# syntax=docker/dockerfile:1

# Build natively on the runner and cross-compile: the SQLite driver is pure Go, so no emulation or C toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/omega .
# Country + network databases (DB-IP Lite, CC BY 4.0), fetched with the app's own updater. These are the
# fallback for a fresh install; the running server downloads each new monthly release into /data by itself.
RUN OMEGA_DB=/tmp/geoip/omega.db go run . geoip-update && \
    mkdir -p /out/geoip /out/data && \
    mv /tmp/geoip/dbip-country-lite-*.mmdb /out/geoip/dbip-country-lite.mmdb && \
    mv /tmp/geoip/dbip-asn-lite-*.mmdb /out/geoip/dbip-asn-lite.mmdb

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/omega /omega
COPY --from=build /out/geoip/ /usr/share/omega/
COPY --from=build --chown=65532:65532 /out/data /data
ENV OMEGA_DB=/data/omega.db \
    OMEGA_GEOIP_DB=/usr/share/omega/dbip-country-lite.mmdb \
    OMEGA_ASN_DB=/usr/share/omega/dbip-asn-lite.mmdb \
    PORT=3300
VOLUME /data
EXPOSE 3300
USER nonroot:nonroot
ENTRYPOINT ["/omega"]
