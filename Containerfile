# Release image, built by GoReleaser (dockers_v2). The binaries are staged in
# the build context as <os>/<arch>/wright, hence the $TARGETPLATFORM copy.
#
# wolfi-base rather than a static base because the agent shells out to git,
# bash, coreutils and ripgrep inside the container. Inside the image the
# container itself is the sandbox boundary (WRIGHT_CONTAINER=1); landlock is
# still requested and used when the host kernel allows it.
FROM cgr.dev/chainguard/wolfi-base:latest

ARG TARGETPLATFORM

RUN apk add --no-cache \
    bash git coreutils findutils grep sed diffutils ripgrep \
    ca-certificates-bundle curl jq \
 && adduser -D -h /home/wright -u 1000 wright \
 && mkdir -p /workspace \
 && chown wright:wright /workspace

COPY $TARGETPLATFORM/wright /usr/local/bin/wright

USER wright
WORKDIR /workspace
ENV HOME=/home/wright \
    WRIGHT_SANDBOX=landlock \
    WRIGHT_CONTAINER=1

ENTRYPOINT ["wright"]
