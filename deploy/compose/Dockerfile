FROM docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS syft
ARG TARGETARCH
RUN case "$TARGETARCH" in \
      amd64) sum=c069905b391cc4c20a5ba65ad5c10be2a7ba074f8ea6ad203e24d14e303dad47 ;; \
      arm64) sum=dfdf0537610113edbefe1f1fc6548bc957b2d77439636ec824fcf0e10d46d054 ;; \
      *) exit 1 ;; \
    esac && \
    wget -qO /syft.tgz "https://github.com/anchore/syft/releases/download/v1.54.1/syft_1.54.1_linux_${TARGETARCH}.tar.gz" && \
    echo "$sum  /syft.tgz" | sha256sum -c - && tar -xzf /syft.tgz -C / syft && mkdir /samuori-data

# Controllers that back up PostgreSQL need pg_dump and pg_restore at least as new as the server; this target carries
# PostgreSQL 18's (docs/operators/backup-restore.md). Build it with --target controller-pgtools.
FROM docker.io/library/postgres:18@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336 AS controller-pgtools
COPY --from=syft /syft /usr/bin/syft
COPY --from=syft --chown=65532:65532 /samuori-data /var/lib/samuori
COPY samuori-server /usr/local/bin/samuori-server
COPY ui /usr/local/share/samuori/ui
ENV SAMUORI_ROLE=controller SAMUORI_DATA_DIR=/var/lib/samuori SAMUORI_UI_DIR=/usr/local/share/samuori/ui SAMUORI_LOG_FORMAT=json
USER 65532:65532
EXPOSE 8080 8444
ENTRYPOINT ["/usr/local/bin/samuori-server"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=syft /syft /usr/bin/syft
COPY --from=syft --chown=65532:65532 /samuori-data /var/lib/samuori
COPY samuori-server /usr/local/bin/samuori-server
COPY ui /usr/local/share/samuori/ui
ENV SAMUORI_ROLE=all-in-one SAMUORI_LISTEN=127.0.0.1:8080 SAMUORI_DATA_DIR=/var/lib/samuori SAMUORI_UI_DIR=/usr/local/share/samuori/ui \
    SAMUORI_WORKER_LISTEN=127.0.0.1:8444 SAMUORI_LOG_FORMAT=json
USER 65532:65532
EXPOSE 8080 8444
ENTRYPOINT ["/usr/local/bin/samuori-server"]
