# The image packages the binary task ci/golang/build leaves in bin/: it holds
# the UI assets and needs no Go toolchain here.
# busybox:1.37
FROM busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec AS data
RUN mkdir -p /rootfs/var/lib/podium \
 && chmod 0700 /rootfs/var/lib/podium \
 && chown 65532:65532 /rootfs/var/lib/podium

# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETARCH
COPY --from=data --chown=65532:65532 --chmod=0700 /rootfs/var/lib/podium /var/lib/podium
COPY --chmod=0555 bin/podium-linux-${TARGETARCH} /podium
ENV SOLTI_CLUSTER_STANDALONE_DATA_DIR=/var/lib/podium \
    SOLTI_OPS_ADDR=:8081
EXPOSE 8080 8081 8082 50051
VOLUME ["/var/lib/podium"]
USER 65532:65532
ENTRYPOINT ["/podium"]
