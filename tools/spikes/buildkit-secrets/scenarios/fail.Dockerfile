FROM docker.io/library/busybox:1.37
RUN --mount=type=secret,id=alpha --mount=type=secret,id=gamma,target=/certificates/gamma.bin \
    echo "hash $(sha256sum /run/secrets/alpha | cut -c1-12)" && exit 3
