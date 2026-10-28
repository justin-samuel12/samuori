FROM docker.io/library/busybox:1.37
RUN --mount=type=secret,id=alpha echo started && sleep 60 && echo finished
