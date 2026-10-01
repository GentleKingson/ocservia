FROM rockylinux:9@sha256:d7be1c094cc5845ee815d4632fe377514ee6ebcf8efaed6892889657e5ddaaa6

# Link the shared DEB/RPM payload against Rocky 9's glibc, not the newer
# Ubuntu runner libc. Rust itself remains pinned by toolchains.lock.
# Abandon mirrors that stay below 512 KiB/s for 15 seconds; keep DNF retries.
RUN dnf --setopt=minrate=512k --setopt=timeout=15 --setopt=max_parallel_downloads=10 \
    install -y gcc gcc-c++ make binutils file git tar gzip xz findutils \
      ca-certificates && dnf clean all
