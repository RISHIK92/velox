# P5-W28-S05 (EXEC-3, EXEC-5; Q-P5-02): gVisor's runsc for the execution hosts, which
# have no internet: built where the deploy runs, pushed to ECR with the stack, and copied
# out at boot. Release 20261019.0 (published 2026-10-22), checked against its pinned
# sha512 before anything is kept.
FROM docker:29.9.2-cli@sha256:5e6baf67325a79f64ed3bed6863954d5e05d0f3d4e57cbdb14809346814bbfdb AS fetch
ARG TARGETARCH
WORKDIR /gvisor
RUN set -eu; \
    case "$TARGETARCH" in \
      arm64) arch=aarch64; sum=6b14b582cd3185169df54275eed8721d94c9b40e6cb1533d0d0c46573271c6d2d03db586ea1caac46a45e805797f00b115bead9fb93636a1ffa69f2430958977 ;; \
      amd64) arch=x86_64; sum=7cbc2aeeee809dcccfb40e612504a1b76f2c119790cee59e482f7b35428cccc4f71705e7e322442d07d1f7d29b5d9e72e31319fc3088d6dcbec9fc7c1f0d8881 ;; \
      *) echo "no gVisor for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    wget -q -O gvisor.tar.bz2 "https://storage.googleapis.com/gvisor/releases/release/20261019.0/$arch/gvisor.tar.bz2"; \
    echo "$sum  gvisor.tar.bz2" | sha512sum -c -; \
    tar -xjf gvisor.tar.bz2; \
    find . -name runsc -type f -exec cp {} /runsc \; ; \
    test -x /runsc

FROM scratch
# Docker runs runsc as its OCI runtime; the containerd shim isn't needed.
COPY --from=fetch /runsc /runsc
