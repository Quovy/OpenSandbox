# Copyright 2026 The OpenSandbox Authors
# 
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# 
#     http://www.apache.org/licenses/LICENSE-2.0
# 
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM golang:1.26.10-alpine3.23 AS builder

WORKDIR /workspace

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG GOFLAGS=
ARG LDFLAGS=
ARG CGO_ENABLED=0
ARG CC=
ARG CXX=
ARG CFLAGS=
ARG CXXFLAGS=
ARG CGO_CFLAGS=
ARG CGO_CXXFLAGS=
ARG CGO_LDFLAGS=
# Optional module proxy for restricted build networks; empty keeps Go's
# environment (proxy.golang.org) untouched.
ARG GOPROXY=

# Copy only go mod/sum first for better caching
COPY components/egress/go.mod components/egress/go.sum ./components/egress/
# Bring internal module so replace ../internal works during download/build
COPY components/internal ./components/internal

WORKDIR /workspace/components/egress

# Static-ish build (no cgo by default) to simplify runtime deps.
RUN test -z "${GOPROXY}" || go env -w GOPROXY="${GOPROXY}" && go mod download

# Pre-download internal-module deps for the supervisor build below.
RUN cd /workspace/components/internal && go mod download

# Copy the rest of the egress sources
COPY components/egress ./
RUN if [ -n "${CC}" ]; then export CC; fi; \
    if [ -n "${CXX}" ]; then export CXX; fi; \
    export CGO_ENABLED="${CGO_ENABLED}" \
           CGO_CFLAGS="${CGO_CFLAGS:-${CFLAGS}}" \
           CGO_CXXFLAGS="${CGO_CXXFLAGS:-${CXXFLAGS}}" \
           CGO_LDFLAGS="${CGO_LDFLAGS}"; \
    go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /out/egress .

# Build the opensandbox-supervisor binary from the internal module.
# Installed alongside /egress so a future ENTRYPOINT switch can wrap egress
# without changing this stage again.
RUN cd /workspace/components/internal && \
    if [ -n "${CC}" ]; then export CC; fi; \
    if [ -n "${CXX}" ]; then export CXX; fi; \
    export CGO_ENABLED="${CGO_ENABLED}" \
           CGO_CFLAGS="${CGO_CFLAGS:-${CFLAGS}}" \
           CGO_CXXFLAGS="${CGO_CXXFLAGS:-${CXXFLAGS}}" \
           CGO_LDFLAGS="${CGO_LDFLAGS}"; \
    go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /out/opensandbox-supervisor ./cmd/supervisor

# egress-trace: read-only admission viewer (policy API + nft sets/leases +
# /proc sockets) for demos and troubleshooting; stdlib-only, same module.
# Run inside the container via docker exec.
RUN go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -B none" \
    -o /out/egress-trace ./tools/egress-trace

FROM cgr.dev/chainguard/wolfi-base@sha256:8acb3a7694eda954b3ad4256aec3291bdd92715d7a8c030abd1a21546337b4e4

# Keep only the packages required by the egress runtime. The prior diagnostic
# toolbelt materially expanded the image's vulnerability surface and is not
# used by the entrypoint or enforcement code. curl stays because the image
# regression tests in egress-test.yml probe the sidecar through `docker exec
# ... curl`. Python is pinned to 3.11: the H2 adapter gate below rejects
# CPython >= 3.12 (mitmproxy enables eager tasks there).
# The version floors keep the compromised emSign ECC Root CA - G3 out of
# /etc/ssl/certs, which mitmproxy uses for upstream TLS, and out of pip's
# vendored certifi.
RUN apk add --no-cache \
        'ca-certificates>=20260909-r3' \
        'ca-certificates-bundle>=20260909-r3' \
        curl \
        iproute2 \
        iptables \
        nftables \
        procps \
        'py3.11-pip>=26.2.2' \
        python-3.11 \
        sudo

# wolfi links iptables to the legacy xtables backend, which needs the
# iptable_nat module and CAP_NET_RAW; the sidecar runs with NET_ADMIN only
# (nat table "does not exist"). Debian's default is the nft backend, so
# match it.
RUN for tool in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do \
        ln -sf xtables-nft-multi /usr/sbin/$tool; \
    done \
    && iptables --version | grep -q nf_tables

# Python mitmproxy (transparent mode): mitmdump runs as user mitmproxy; iptables skips this uid.
# /var/lib/mitmproxy is mitm's home, used as the confdir (CA + config.yaml live under .mitmproxy/).
COPY components/egress/requirements-mitmproxy.txt /tmp/requirements-mitmproxy.txt
RUN addgroup -S -g 10042 mitmproxy \
    && adduser -S -D -H -u 10042 -G mitmproxy -h /var/lib/mitmproxy -s /sbin/nologin mitmproxy \
    && mkdir -p /var/lib/mitmproxy/.mitmproxy \
    && chown -R mitmproxy:mitmproxy /var/lib/mitmproxy \
    && pip3.11 install --no-cache-dir -r /tmp/requirements-mitmproxy.txt 'certifi>=2026.10.19' \
    && (command -v mitmdump && mitmdump --version) \
    && rm -rf /root/.cache \
    && mkdir -p /var/egress/mitmscripts

# mitmproxy 11.0.2 (held exact by the credential_bound_h2 gate) declares
# h11<=0.14.0 and Brotli<=1.1.0, both with Critical CVEs that the publish
# workflow's Trivy gate rejects (CVE-2025-43859, CVE-2026-111497). mitmproxy
# 12.x pins exactly these fixed versions, and the egress suite passes with
# them, so they are installed past the declared bound. Re-check when the
# mitmproxy pin moves.
RUN pip3.11 install --no-cache-dir --no-deps h11==0.16.0 Brotli==1.2.1 \
    && rm -rf /root/.cache

# Static mitmproxy options (mode, listen_host, connection_strategy, stream_large_bodies,
# http2, ignore_hosts, ssl_verify_upstream_trusted_confdir). mitmdump auto-loads
# config.yaml from its confdir. Dynamic per-deployment options stay env-driven and
# are applied as --set by launch.go (which overrides values declared here).
COPY components/egress/mitmproxy/config.yaml /var/lib/mitmproxy/.mitmproxy/config.yaml
RUN chown mitmproxy:mitmproxy /var/lib/mitmproxy/.mitmproxy/config.yaml \
    && chmod 0644 /var/lib/mitmproxy/.mitmproxy/config.yaml

# All egress runtime artifacts live under one directory to keep paths grouped.
COPY --from=builder /out/egress /opt/opensandbox-egress/egress
COPY --from=builder /out/opensandbox-supervisor /opt/opensandbox-egress/supervisor
COPY --from=builder /out/egress-trace /opt/opensandbox-egress/egress-trace
# Pre-start hook: reap any mitmdump left over from a previous crashed
# egress, and remove stale DNS-redirect iptables rules + opensandbox_dns_redirect
# nft tables left pointing at a dead proxy. The `inet opensandbox` policy table
# is intentionally NOT touched — the nftables manager prepends its own delete
# and recreates the table on every ApplyStatic, so it is idempotent.
COPY components/egress/scripts/cleanup.sh /opt/opensandbox-egress/cleanup.sh
# wolfi-base ships no /usr/local/bin; create it before the egress-trace link.
RUN chmod 0755 /opt/opensandbox-egress/cleanup.sh \
            /opt/opensandbox-egress/egress \
            /opt/opensandbox-egress/supervisor \
            /opt/opensandbox-egress/egress-trace \
    && ln -s /opt/opensandbox-egress/egress /egress \
    && mkdir -p /usr/local/bin \
    && ln -s /opt/opensandbox-egress/egress-trace /usr/local/bin/egress-trace

COPY components/egress/mitmscripts /var/egress/mitmscripts
# The mitmdump addon is loaded by the mitmproxy user (uid 10042): normalize
# permissions so a restrictive build umask (e.g. 027) can never leave the
# scripts unreadable to it. Check the installed libraries against the same
# interpreter and exact-version gate used by the H2 adapter before producing
# the image.
RUN chmod -R a+rX /var/egress/mitmscripts \
    && PYTHONPATH=/var/egress/mitmscripts python3 -c \
        'import sys; from credential_bound_h2 import PINS, dependency_versions_supported; \
         raise SystemExit(0 if dependency_versions_supported() else f"unsupported H2 runtime: interpreter={sys.implementation.name}, sys.version={sys.version!r}; required CPython 3.10/3.11 and packages={PINS}")'

# Supervisor wraps the egress binary: restarts on crash with backoff and
# forwards SIGTERM gracefully. The cleanup hook runs only as pre-start; the
# next egress process re-installs all redirect rules itself after startup,
# so there is nothing to run on post-exit.
# Expects OPENSANDBOX_NETWORK_POLICY env at runtime.
ENTRYPOINT ["/opt/opensandbox-egress/supervisor", \
            "--pre-start=/opt/opensandbox-egress/cleanup.sh", \
            "--name=egress", \
            "--grace-period=20s", \
            "--", \
            "/opt/opensandbox-egress/egress"]
