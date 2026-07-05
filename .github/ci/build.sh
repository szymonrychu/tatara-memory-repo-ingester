#!/usr/bin/env bash
# Build this repo's image via the shared rootless buildkitd daemon and push to
# harbor. Runs on the ARC runner (in-cluster, namespace arc-runners). Talks gRPC
# to the buildkitd Service; buildkitd writes all layers/cache to its Ceph PVC
# (--root), OFF the control-plane etcd NVMe. No in-cluster Job, no transient
# cluster secrets: harbor push auth is a per-build docker config on THIS runner,
# the private-repo clone token is a buildkit frontend secret. Replaces
# kaniko-build.sh.
set -euo pipefail

REPO="${1:?repo name required}"
BUILDKITD_ADDR="tcp://buildkitd.arc-runners:1234"
SHORT_SHA="${GITHUB_SHA:0:7}"
VERSION="$(git describe --tags --always --dirty)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
DEST="harbor.szymonrichert.pl/containers/${REPO}"

: "${GITHUB_TOKEN:?GITHUB_TOKEN required}"
: "${HARBOR_USERNAME:?HARBOR_USERNAME required}"
: "${HARBOR_PASSWORD:?HARBOR_PASSWORD required}"

# ci.yml pushes both :SHORT_SHA (rollback traceability) and :VERSION on every
# push to main. release.yml re-invokes this script on the SAME commit to
# republish at the final :vX.Y.Z tag; Harbor's containers project has tag
# immutability, so re-pushing :SHORT_SHA (already pushed by ci.yml) 412s.
# release.yml sets PUSH_SHORT_SHA_TAG=false to push only :VERSION.
if [ "${PUSH_SHORT_SHA_TAG:-true}" = "true" ]; then
  TAGS="${DEST}:${SHORT_SHA},${DEST}:${VERSION}"
else
  TAGS="${DEST}:${VERSION}"
fi

# Per-build docker config on the runner only (never an in-cluster secret).
# buildctl reads $DOCKER_CONFIG and forwards harbor auth to buildkitd for push.
DOCKER_CONFIG="$(mktemp -d)"
export DOCKER_CONFIG
trap 'rm -rf "$DOCKER_CONFIG"' EXIT
auth="$(printf '%s:%s' "$HARBOR_USERNAME" "$HARBOR_PASSWORD" | base64 -w0)"
cat >"${DOCKER_CONFIG}/config.json" <<EOF
{"auths":{"harbor.szymonrichert.pl":{"auth":"${auth}"}}}
EOF

# Remote git context (buildkitd clones the private repo, like kaniko did).
# MUST be https:// (NOT git://): buildkit's GIT_AUTH_TOKEN basic-auth extraheader
# only engages over https, and github.com no longer serves the git:// protocol.
# GIT_AUTH_TOKEN is the buildkit git-source frontend secret for the private
# clone; it is NOT a build-arg, so it never lands in a layer.
buildctl --addr "$BUILDKITD_ADDR" build \
  --frontend dockerfile.v0 \
  --opt context="https://github.com/szymonrychu/${REPO}.git#${GITHUB_SHA}" \
  --opt filename=Dockerfile \
  --opt build-arg:VERSION="${VERSION}" \
  --opt build-arg:COMMIT="${SHORT_SHA}" \
  --opt build-arg:DATE="${BUILD_DATE}" \
  --secret id=GIT_AUTH_TOKEN,env=GITHUB_TOKEN \
  --output "type=image,\"name=${TAGS}\",push=true"

echo "buildkit: pushed ${TAGS}"
