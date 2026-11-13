# Release image for Stratus. GoReleaser (dockers_v2) builds it for each
# platform with a context holding linux/<arch>/stratus; CI builds it the same
# way from a locally compiled binary.
FROM alpine:3.22

ARG TARGETPLATFORM

# The Alpine base already ships the CA bundle (ca-certificates-bundle) that
# Bedrock record mode and other outbound HTTPS calls need.
COPY ${TARGETPLATFORM}/stratus /usr/local/bin/stratus

# Runs as root so a mounted /var/run/docker.sock works for the
# Docker-backed services (Lambda, RDS, ElastiCache, MSK).
WORKDIR /var/lib/stratus
VOLUME ["/var/lib/stratus"]
EXPOSE 4566

HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 \
    CMD wget -qO- http://127.0.0.1:4566/_stratus/health >/dev/null || exit 1

ENTRYPOINT ["stratus"]
CMD ["serve", "--port", "4566", "--data-dir", "/var/lib/stratus", "--log-format", "json"]
