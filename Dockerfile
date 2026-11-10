# The image packages the binary task ci/golang/build leaves in bin/: it holds
# the UI assets and needs no Go toolchain here.
# busybox:1.37
FROM --platform=$BUILDPLATFORM busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec AS data
# The data directory, mode 0700, copied below as an entry of /rootfs/: a copied
# entry keeps its mode in every BuildKit version, while a --chmod on the COPY
# does not reach a directory the COPY creates in older ones (BuildKit v0.20 left
# it 0755, which the durable store refuses).
RUN mkdir -p /rootfs/podium && chmod 0700 /rootfs/podium

# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETARCH
COPY --from=data --chown=65532:65532 /rootfs/ /var/lib/
COPY --chmod=0555 bin/podium-linux-${TARGETARCH} /podium
ENV SOLTI_CLUSTER_STANDALONE_DATA_DIR=/var/lib/podium \
    SOLTI_OPS_ADDR=:8081
EXPOSE 8080 8081 8082 50051
VOLUME ["/var/lib/podium"]
USER 65532:65532
ENTRYPOINT ["/podium"]
