# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# The Supervisor and its collector in a container, from the release archives: build.sh unpacks
# them into the build context (see it for how to build). No shell and no package manager: the
# binaries are static, and distroless gives them CA certificates, /tmp and a non-root user.

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

ARG TARGETARCH
ARG VERSION

LABEL org.opencontainers.image.title="pydantic OpAMP Supervisor" \
      org.opencontainers.image.description="The OpAMP Supervisor and an OpenTelemetry Collector with the opamp extension" \
      org.opencontainers.image.source="https://github.com/pydantic/opamp-clients" \
      org.opencontainers.image.licenses="MIT AND Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

COPY linux_${TARGETARCH}/opampsupervisor linux_${TARGETARCH}/otelcol /usr/lib/pydantic-opamp/
COPY licenses/ /usr/share/doc/pydantic-opamp/
COPY supervisor.yaml supervisor-restricted.yaml /etc/pydantic-opamp/
# The state directory, the container's user's: a volume mounted here starts with its owner.
COPY --chown=65532:65532 state/ /var/lib/pydantic-opamp/

USER 65532:65532
VOLUME /var/lib/pydantic-opamp
ENTRYPOINT ["/usr/lib/pydantic-opamp/opampsupervisor", "--config", "/etc/pydantic-opamp/supervisor.yaml"]
