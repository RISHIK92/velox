# P5-W28-S03 (EXEC-2): Go's runner image. A cold `go build` in the sandbox takes about
# 40 s, so the cache for the packages a harness and typical solutions import is built
# here; each run copies it into its own /tmp, which keeps a build to 2-3 s. On the game
# server's pinned base (services/game/Dockerfile).
FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb
COPY warm.go /opt/warm/main.go
RUN cd /opt/warm && GOCACHE=/opt/gocache CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /dev/null main.go \
    && chmod -R a+rX /opt/gocache && rm -r /opt/warm
