# Copyright 2025 Alibaba Group Holding Ltd.
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

FROM golang:1.25.12@sha256:dbeddb5e728ea4b5ba7920574413e311025ffa42d196793857e4d160e8dfe60d AS builder

WORKDIR /build

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

# Prepare local modules to satisfy replace directives.
COPY components/internal/go.mod components/internal/go.sum ./components/internal/
COPY components/execd/go.mod components/execd/go.sum ./components/execd/

# Download deps with only mod files for better caching.
RUN cd components/internal && go mod download
RUN cd components/execd && go mod download

# Copy sources.
COPY components/internal ./components/internal
COPY components/execd ./components/execd

WORKDIR /build/components/execd

# Build the opensandbox-supervisor binary from the internal module.
RUN cd /build/components/internal && \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /build/opensandbox-supervisor ./cmd/supervisor

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
    -o /build/execd ./main.go

RUN CGO_ENABLED=0 GOOS=windows go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /build/execd.exe ./main.go

# Build static bubblewrap with musl.
FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS bwrap-builder
RUN apk add --no-cache git musl-dev meson ninja gcc libcap-dev libcap-static pkgconfig bash
COPY components/execd/native/session-gate.c /build/session-gate.c
RUN gcc -Os -static -s -Wall -Wextra -Werror \
    -o /build/opensandbox-session-gate /build/session-gate.c
RUN git clone --depth 1 --branch v0.11.2 \
    https://github.com/containers/bubblewrap /build/bwrap
WORKDIR /build/bwrap
RUN rm /usr/lib/libcap.so /usr/lib/libcap.so.2 && \
    meson setup builddir \
    -Dprefix=/usr \
    -Dman=disabled \
    -Dtests=false \
    -Dsupport_setuid=false \
    -Ddefault_library=static \
    -Dc_link_args='-static' && \
    ninja -C builddir && \
    cp builddir/bwrap /build/bwrap/bwrap

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

COPY --from=bwrap-builder /build/bwrap/bwrap /usr/local/bin/bwrap
COPY --from=bwrap-builder --chown=0:0 --chmod=0555 /build/opensandbox-session-gate /usr/local/libexec/opensandbox-session-gate
COPY --from=bwrap-builder --chown=0:0 --chmod=0555 /build/opensandbox-session-gate /opt/opensandbox/opensandbox-session-gate
COPY --from=builder /build/execd .
COPY --from=builder /build/execd.exe ./execd.exe
COPY --from=builder /build/opensandbox-supervisor ./opensandbox-supervisor
COPY components/execd/bootstrap.sh ./bootstrap.sh
COPY components/execd/install.bat ./install.bat

ENTRYPOINT ["./execd"]
