FROM docker.io/library/busybox:1.37 AS unused
RUN --mount=type=secret,id=delta cat /run/secrets/delta > /delta

FROM docker.io/library/busybox:1.37
RUN echo step1 && sleep 2
RUN --mount=type=secret,id=alpha sleep 2 && echo "alpha $(sha256sum /run/secrets/alpha | cut -c1-12)"
RUN --mount=type=secret,id=beta --mount=type=secret,id=gamma,target=/certificates/gamma.bin sleep 2 && \
    echo "beta $(sha256sum /run/secrets/beta | cut -c1-12) gamma $(sha256sum /certificates/gamma.bin | cut -c1-12)" && \
    ls -la /run/secrets /certificates
RUN --mount=type=secret,id=alpha sleep 1 && echo "alpha-again $(sha256sum /run/secrets/alpha | cut -c1-12)"
RUN --mount=type=secret,id=epsilon,required=false echo "optional-missing: $(ls -A /run/secrets 2>&1 | tr '\n' ' ')"
RUN --mount=type=secret,id=omega echo "default-required-missing: $(ls -A /run/secrets 2>&1 | tr '\n' ' ')"
RUN echo "after: $(ls -la /run/secrets /certificates 2>&1 | tr '\n' ' ')"
