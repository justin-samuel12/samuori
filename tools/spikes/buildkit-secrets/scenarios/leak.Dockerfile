FROM docker.io/library/busybox:1.37
RUN --mount=type=secret,id=alpha cp /run/secrets/alpha /copied-by-build-script
