FROM docker.io/library/busybox:1.37
RUN echo "secret-independent step" > /pre && sleep 1
RUN --mount=type=secret,id=alpha sh -c 'sha256sum /run/secrets/alpha | cut -c1-12 > /derived'
RUN cat /derived
