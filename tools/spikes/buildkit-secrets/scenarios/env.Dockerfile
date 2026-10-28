FROM docker.io/library/busybox:1.37
RUN --mount=type=secret,id=alpha,env=ALPHA_VALUE sh -c 'echo "env length ${#ALPHA_VALUE}"'
