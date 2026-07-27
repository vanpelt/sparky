FROM docker.io/library/ubuntu:24.04@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bc \
        bison \
        build-essential \
        ca-certificates \
        curl \
        flex \
        libelf-dev \
        libssl-dev \
        python3 \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Keep the large, immutable inputs beside the toolchain. A cache miss pulls one
# digest-pinned image instead of making both ARM runners download kernel.org
# independently. The build verifies these hashes again before every compile.
ARG LINUX_VERSION
ARG LINUX_URL
ARG LINUX_SHA256
ARG APPLE_CONFIG_TAG
ARG APPLE_CONFIG_URL
ARG APPLE_CONFIG_SHA256

RUN mkdir -p /kernel-inputs \
    && curl --fail --location --silent --show-error \
        --proto '=https' --tlsv1.2 --retry 3 \
        --output "/kernel-inputs/linux-${LINUX_VERSION}.tar.xz" \
        "${LINUX_URL}" \
    && echo "${LINUX_SHA256}  /kernel-inputs/linux-${LINUX_VERSION}.tar.xz" \
        | sha256sum --check --strict \
    && curl --fail --location --silent --show-error \
        --proto '=https' --tlsv1.2 --retry 3 \
        --output "/kernel-inputs/apple-config-arm64-${APPLE_CONFIG_TAG}" \
        "${APPLE_CONFIG_URL}" \
    && echo "${APPLE_CONFIG_SHA256}  /kernel-inputs/apple-config-arm64-${APPLE_CONFIG_TAG}" \
        | sha256sum --check --strict

ENV SPARKBOX_KERNEL_TOOLCHAIN_READY=1 \
    SPARKBOX_KERNEL_INPUTS_IN_IMAGE=1
