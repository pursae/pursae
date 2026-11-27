# Base images are pinned by digest (the multi-platform index of the tag) so a build is reproducible;
# Dependabot (.github/dependabot.yml) proposes new digests weekly.
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Seed IP-to-country database: DB-IP "IP to Country Lite" (CC BY 4.0, https://db-ip.com), this month's
# release or, early in the month, last month's. Optional: without network the image ships none and the
# site downloads it itself (internal/server/geoip.go).
RUN mkdir -p /out/geoip && y=$(date -u +%Y) && m=$(date -u +%m) && m=${m#0} \
	&& if [ "$m" -eq 1 ]; then prev="$((y-1))-12"; else prev="$y-$(printf %02d $((m-1)))"; fi \
	&& for v in "$y-$(printf %02d $m)" "$prev"; do \
		wget -q -T 60 -O - "https://download.db-ip.com/free/dbip-country-lite-$v.mmdb.gz" 2>/dev/null | gunzip > /out/geoip/country.mmdb.tmp 2>/dev/null \
		&& [ -s /out/geoip/country.mmdb.tmp ] && mv /out/geoip/country.mmdb.tmp /out/geoip/country.mmdb && break; \
	done; rm -f /out/geoip/country.mmdb.tmp; true
COPY . .
# VERSION (optional) is the core version the binary reports, e.g. --build-arg VERSION=0.4.0.
ARG VERSION=
# ROOT_KEYS (optional): comma-separated base64 Ed25519 release root keys ADDED to the Pursae release
# root every build pins (key rotation); only Pursae Manager keys endorsed by a root verify a core
# self-update or a Manager-signed plugin (internal/server/hub_trust.go).
ARG ROOT_KEYS=
# GO_TAGS (optional): extra build tags. "devhub" builds a development binary that reads
# PURSAE_DEV_HUB_URL (a local Pursae Manager) and PURSAE_TRUST_HUB_KEYS; production images leave it empty,
# so the binary always talks to https://hub.pursae.com and trusts only endorsed keys.
ARG GO_TAGS=
# A static binary (no ELF interpreter), so the same file runs on musl and glibc systems:
# -buildmode=exe, and -tags nodynamic keeps github.com/gen2brain/avif off purego (it decodes AVIF
# with its embedded WebAssembly build instead of a system libavif).
# "paydemo" (the development-only demo bank of Pursae Commerce, which accepts http on loopback) must
# never be in an image.
RUN case " $GO_TAGS " in *paydemo*) echo "GO_TAGS must not contain paydemo: it is for local development only" >&2; exit 1;; esac; \
	CGO_ENABLED=0 go build -trimpath -buildmode=exe -tags "nodynamic $GO_TAGS" -ldflags="-s -w${VERSION:+ -X wpgo/internal/server.Version=$VERSION}${ROOT_KEYS:+ -X wpgo/internal/server.rootKeysLinked=$ROOT_KEYS}" -o /out/wpgo ./cmd/wpgo

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 wpgo \
	&& mkdir -p /data/uploads /data/plugins /data/plugin-cache /data/mail /data/themes /data/wpgo-cache /data/languages /data/bin /data/backups /data/geoip \
	&& chown -R wpgo /data
# The image's binary; `wpgo launch` installs it into /data/bin (a volume) and runs it from there, so
# Dashboard › Updates can replace it and the update survives restarts, while a newer image still wins.
COPY --from=build /out/wpgo /usr/local/lib/pursae/wpgo
RUN ln -s /usr/local/lib/pursae/wpgo /usr/local/bin/wpgo \
	&& ln -s /usr/local/lib/pursae/wpgo /usr/local/bin/pursae
# The seed country database (may be absent); /data/geoip/country.mmdb, refreshed monthly, wins over it.
COPY --from=build /out/geoip/ /usr/share/wpgo/geoip/
# License and third-party notices of everything the binary bundles (NOTICE lists the components).
COPY LICENSE NOTICE /usr/share/doc/pursae/
COPY LICENSES/ /usr/share/doc/pursae/LICENSES/
# Demo marketplace: example plugin packages built by tools/build-plugins.sh (if present).
COPY examples/plugins/dis[t]/*.zip examples/plugins/dis[t]/dev-key.pub /usr/share/wpgo/plugins/
USER wpgo
# WPGO_BIN_DIR is the name releases before 0.5.0 read: a core update rollback may run one of them from
# /data/bin. The same value under both names is not reported as a deprecated setting.
ENV PURSAE_BIN_DIR=/data/bin WPGO_BIN_DIR=/data/bin
# The installer asks for a setup token: PURSAE_SETUP_TOKEN, or the one the server prints in its log on a
# fresh site (`docker exec <container> wpgo setup-token` prints a new install link).
EXPOSE 8080
# Liveness: GET /healthz on PURSAE_ADDR through the binary itself (no curl in the image). The start
# period covers the migrations a new version runs before it listens.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD ["/usr/local/lib/pursae/wpgo", "healthcheck"]
ENTRYPOINT ["/usr/local/lib/pursae/wpgo", "launch"]
CMD ["serve"]
