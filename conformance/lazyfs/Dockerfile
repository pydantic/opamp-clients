# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# The crash scenarios on a real file system that loses what wasn't flushed: LazyFS (FUSE), and
# what builds and runs one driver per runtime natively (Rust, Go, Node.js, Python, Java, .NET), so
# every driver's state file is on a LazyFS mount of this container. conformance/lazyfs/run.sh
# builds it and runs it with /dev/fuse and CAP_SYS_ADMIN; docs/state-file.md says what they are for.

FROM eclipse-temurin:21-jdk AS jdk
FROM maven:3.9-eclipse-temurin-21 AS maven
FROM mcr.microsoft.com/dotnet/sdk:8.0 AS dotnet
FROM ghcr.io/astral-sh/uv:0.9 AS uv

FROM golang:1.25-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential cmake git ca-certificates curl xz-utils pkg-config \
        fuse3 libfuse3-dev python3 libicu72 libssl3 zlib1g procps \
    && rm -rf /var/lib/apt/lists/* \
    && echo user_allow_other >> /etc/fuse.conf

# LazyFS at the commit after 0.3.1 that lets a torn-op fault sent through the FIFO name its
# occurrence (dsrhaslab/lazyfs#17).
ARG LAZYFS_COMMIT=fa7d32ee9710d55a7e529ac3c375f6616563c78b
RUN git clone https://github.com/dsrhaslab/lazyfs.git /opt/lazyfs \
    && cd /opt/lazyfs && git checkout -q "$LAZYFS_COMMIT" \
    && cd libs/libpcache && ./build.sh >/dev/null \
    && cd ../../lazyfs && ./build.sh >/dev/null \
    && test -x /opt/lazyfs/lazyfs/build/lazyfs
ENV LAZYFS=/opt/lazyfs/lazyfs/build/lazyfs \
    LD_LIBRARY_PATH=/opt/lazyfs/libs/libpcache/build

# Rust, as rust-toolchain.toml pins it, for what is built from the core's sources where they are.
ENV RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo PATH=/opt/cargo/bin:$PATH
RUN curl -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal --default-toolchain 1.95.0 \
        -c rustfmt -c clippy -t wasm32-unknown-unknown

# Node.js 24.
ARG NODE_VERSION=24.9.0
RUN arch="$(dpkg --print-architecture | sed 's/amd64/x64/')" \
    && curl -sSfL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.xz" \
       | tar -xJ -C /usr/local --strip-components=1

# Python 3.13 through uv.
COPY --from=uv /uv /uvx /usr/local/bin/
ENV UV_PYTHON=3.13 UV_PYTHON_INSTALL_DIR=/opt/uv-python
RUN uv python install 3.13

# Java 21 and Maven; .NET 8.
COPY --from=jdk /opt/java/openjdk /opt/java/openjdk
COPY --from=maven /usr/share/maven /usr/share/maven
COPY --from=dotnet /usr/share/dotnet /usr/share/dotnet
ENV JAVA_HOME=/opt/java/openjdk DOTNET_ROOT=/usr/share/dotnet DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 PATH=/opt/java/openjdk/bin:/usr/share/maven/bin:/usr/share/dotnet:$PATH
RUN ln -s /usr/share/maven/bin/mvn /usr/local/bin/mvn && ln -s /usr/share/dotnet/dotnet /usr/local/bin/dotnet

WORKDIR /work
