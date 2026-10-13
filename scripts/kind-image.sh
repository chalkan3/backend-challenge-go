#!/usr/bin/env bash
#
# Builds the application image locally and puts it inside the kind node.
#
# Two steps, and the second one is the point:
#
#   1. build with the local engine (podman or docker)
#   2. `kind load docker-image` — the tarball is streamed into the node's
#      containerd. kind, being a single-node cluster on a laptop, does have a
#      real Docker/Podman daemon under it, and that daemon can be pointed at
#      directly:  `kind load image-archive` is the variant that works when the
#      daemon is reachable but `docker save` is not.
#
# Why this instead of a registry: no registry exists on the machine, and
# standing one up (or a `kind-registry` container) is a second moving part for
# zero benefit when the only consumer is this cluster. It is not hypothetical
# either — a plain `docker build` + `helm install` would leave the pod in
# ImagePullBackOff, because the kubelet resolves `wallet:sha` at the node, and
# the node has never seen that tag.
#
# The tag is the short git SHA, never "latest": with imagePullPolicy=IfNotPresent
# a reused tag would mean a redeploy of changed code silently keeps running the
# old image, which is the classic way to lose an afternoon.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-wallet}"
CONTAINERFILE="${CONTAINERFILE:-Dockerfile}"
PLATFORM="${PLATFORM:-}"

command -v kind >/dev/null 2>&1 || { echo "kind-image: kind not found; run 'make kind-check'" >&2; exit 1; }

# Same engine detection as kind-check.sh, kept local so this script is runnable
# on its own.
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
	ENGINE=(docker)
elif command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then
	ENGINE=(podman)
else
	echo "kind-image: no usable container engine (docker/podman)" >&2
	exit 1
fi

# Repository name. This is the single most expensive gotcha in this whole path,
# so it is resolved by asking the engine rather than by guessing.
#
# Podman refuses to store an image under a bare, unqualified name: `wallet:sha`
# becomes `localhost/wallet:sha` (`localhost/` is its marker for "local name,
# never resolve against a registry"). Whichever name the engine *actually*
# stored is the name the node's containerd will hold after `kind load`, and it
# is the only name the kubelet can resolve.
#
# If the Deployment asks for a name the node does not have, containerd
# qualifies it to `docker.io/library/<name>` and tries to pull it from Docker
# Hub. That is exactly the observed failure: the pod sits in ImagePullBackOff
# with `docker.io/library/wallet:sha`, while `crictl images` inside the node
# happily shows `localhost/wallet:sha` is present. Reading the name back from
# the engine removes the guess entirely.
#
# Overridable via IMAGE_REPO for a real registry, where the qualifier is the
# registry host and no rewriting happens.
if [[ -z "${IMAGE_REPO:-}" ]]; then
	if [[ "${ENGINE[0]}" == "podman" ]]; then
		IMAGE_REPO="localhost/wallet"
	else
		# `docker` here can still be Podman's docker-compat shim (nixpkgs ships
		# podman-docker-compat as `docker`), in which case it answers `docker
		# info` but rewrites the name exactly like podman. So the bare name is
		# only a *candidate* — the authoritative value is read back below.
		IMAGE_REPO="wallet"
	fi
fi

# Immutable tag: git SHA, with a -dirty suffix so an uncommitted tree is never
# mistaken for the committed revision.
GIT_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
if ! git diff --quiet 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
	GIT_SHA="${GIT_SHA}-dirty"
fi
TAG="${TAG:-$GIT_SHA}"
IMAGE="${IMAGE_REPO}:${TAG}"

args=(build -t "$IMAGE" -f "$CONTAINERFILE" .)
[[ -n "$PLATFORM" ]] && args+=(--platform "$PLATFORM")

echo "kind-image: building $IMAGE with ${ENGINE[0]}"
"${ENGINE[@]}" "${args[@]}"

# Now read back the name the engine really stored. `--format` with the bare
# repository+tag is supported by both podman and docker, and it is the only
# trustworthy answer: it returns `localhost/wallet:sha` under podman (and under
# the docker-compat shim), and `wallet:sha` under real Docker.
if [[ -z "${IMAGE_REPO_FORCED:-}" ]]; then
	resolved="$("${ENGINE[@]}" images --format '{{.Repository}}:{{.Tag}}' \
		| grep -Fx "$IMAGE" | head -1 || true)"
	if [[ -z "$resolved" ]]; then
		# The engine rewrote the name. Ask for whatever points at the image ID
		# we just built, preferring a `localhost/`-qualified match, since that
		# is what an unqualified build tag becomes.
		img_id="$("${ENGINE[@]}" images -q "$IMAGE" | head -1 || true)"
		if [[ -n "$img_id" ]]; then
			resolved="$("${ENGINE[@]}" images --format '{{.Repository}}:{{.Tag}}' \
				| grep -F "$TAG" | grep -F 'localhost/wallet' | head -1 || true)"
			[[ -z "$resolved" ]] && resolved="$("${ENGINE[@]}" images \
				--format '{{.Repository}}:{{.Tag}}' --filter "dangling=false" \
				| grep -F "$TAG" | head -1 || true)"
		fi
	fi
	if [[ -n "$resolved" ]]; then
		if [[ "$resolved" != "$IMAGE" ]]; then
			echo "kind-image: engine stored the image as '$resolved' (requested '$IMAGE')"
			echo "kind-image: using '$resolved' — the node will hold exactly this name"
		fi
		IMAGE="$resolved"
	else
		echo "kind-image: warning: could not read back the stored image name;" >&2
		echo "           falling back to '$IMAGE'. If the pod lands in" >&2
		echo "           ImagePullBackOff, run: ${ENGINE[0]} images | grep wallet" >&2
	fi
fi

echo "kind-image: loading $IMAGE into cluster '$CLUSTER_NAME'"
kind load docker-image "$IMAGE" --name "$CLUSTER_NAME"

# Exported for kind-deploy.sh through a file, since a child process cannot
# export into its parent shell. Both halves are written out split, because the
# previous `--set image.repository=${IMAGE%%:*}` mangled any repository that
# itself contains a colon (a registry with a port, e.g. `localhost:5000/wallet`)
# and silently sent Helm the wrong name.
{
	printf 'IMAGE=%s\n' "$IMAGE"
	printf 'IMAGE_REPOSITORY=%s\n' "${IMAGE%:*}"
	printf 'IMAGE_TAG=%s\n' "${IMAGE##*:}"
} > .kind-image
echo "kind-image: done -> $IMAGE (recorded in .kind-image)"
