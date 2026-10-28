ARG BASE=docker.io/library/busybox:1.37
ARG NET_BASE=docker.io/curlimages/curl:latest

FROM ${BASE} AS ident
RUN echo "== identity" && id && cat /proc/self/uid_map && \
    grep -E '^(Seccomp|NoNewPrivs|CapEff|CapBnd):' /proc/self/status && \
    echo "== kernel" && uname -r && \
    echo "== host paths" && \
    for p in /var/run/docker.sock /run/containerd/containerd.sock /run/buildkit /dev/otel-grpc.sock /dev/kvm; do \
      [ -e "$p" ] && echo "PRESENT $p" || echo "absent $p"; done && \
    touch /ident-done

FROM ${NET_BASE} AS net
ARG HOST_IP
USER root
RUN echo "== network" && \
    probe() { curl -sf -o /dev/null -m 5 $3 "$2" && echo "REACHED $1" || echo "blocked $1 (curl $?)"; } && \
    probe metadata http://169.254.169.254/latest/meta-data/ && \
    probe direct-https https://example.com/ && \
    probe host-loopback http://10.0.2.2:18080/ && \
    probe host-ip "http://${HOST_IP}:18080/" && \
    (nslookup example.com >/dev/null 2>&1 && echo "REACHED dns" || echo "blocked dns") && \
    probe npm-via-proxy https://registry.npmjs.org/ "-x http://${HOST_IP}:8888" && \
    probe denied-host-via-proxy https://example.org/ "-x http://${HOST_IP}:8888" && \
    probe metadata-via-proxy http://169.254.169.254/ "-x http://${HOST_IP}:8888" && \
    touch /net-done

FROM ${BASE}
COPY --from=ident /ident-done /tmp/
COPY --from=net /net-done /tmp/
RUN --mount=type=secret,id=probe_secret \
    echo "== secret" && \
    ( [ -s /run/secrets/probe_secret ] && echo "secret mounted" || echo "secret missing" )
RUN echo "== limits" && (cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/pids.max 2>/dev/null || echo "no cgroup view") && \
    df -h / | tail -1
