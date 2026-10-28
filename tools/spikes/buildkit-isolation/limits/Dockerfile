FROM docker.io/library/busybox:1.37
RUN echo "== pids" && i=0; while [ $i -lt 600 ]; do sleep 30 & i=$((i+1)); done 2>/dev/null; echo "started $(jobs -p | wc -l) background processes"
RUN echo "== memory" && (head -c 3000m /dev/zero | tail >/dev/null && echo "ALLOCATED 3000m" || echo "memory allocation stopped (exit $?)")
