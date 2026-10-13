#!/usr/bin/env bash
#
# Preflight for the local Kubernetes path.
#
# The point of this script is to fail *before* anything is created, with a
# message that says what to install — not to blow up halfway through a deploy
# when `kind` turns out to be missing. Every requirement is checked and
# reported, so one run tells you everything that is absent instead of one
# missing binary at a time.
#
# It also decides which container engine to use and exports it, so the other
# scripts do not each re-implement the detection.
set -euo pipefail

for bin in kind helm kubectl; do
	if ! command -v "$bin" >/dev/null 2>&1; then
		printf 'kind-check: missing required tool: %s\n' "$bin" >&2
		MISSING=1
	fi
done
if [[ "${MISSING:-0}" == "1" ]]; then
	cat >&2 <<'EOF'

Install the missing tools, for example:
  kind    https://kind.sigs.k8s.io/docs/user/quick-start/#installation
  helm    https://helm.sh/docs/intro/install/
  kubectl https://kubernetes.io/docs/tasks/tools/

On NixOS / nix with flakes:  nix profile install nixpkgs#{kind,kubernetes-helm,kubectl}
On macOS (brew):             brew install kind helm kubectl
EOF
	exit 1
fi

# Engine detection. kind defaults to Docker; when only Podman is present it
# needs KIND_EXPERIMENTAL_PROVIDER=podman. Note that the `docker` on PATH can
# be a shim over podman (some distros package it that way), so the real check
# is whether the docker *daemon* answers, not whether the binary exists.
engine=""
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
	engine=docker
elif command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then
	engine=podman
	export KIND_EXPERIMENTAL_PROVIDER=podman
	# Rootless podman needs cgroup v2 delegation for the kubelet to manage
	# pod cgroups; cgroup v1 or a delegated-less setup fails at kubeadm time
	# with a message that is hard to trace back to the cause.
	if [[ "$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" == "true" ]]; then
		if [[ ! -f /sys/fs/cgroup/cgroup.controllers ]]; then
			echo "kind-check: warning: podman is rootless but cgroup v2 is not mounted;" >&2
			echo "            the kubelet may fail to start (needs cgroup v2 + delegation)." >&2
		fi
	fi
else
	cat >&2 <<'EOF'
kind-check: no usable container engine found.

kind needs Docker or Podman. Start one of them and retry:
  docker info
  podman info
EOF
	exit 1
fi

echo "kind-check: ok"
echo "  kind    $(kind version | awk '{print $2}')"
echo "  helm    $(helm version --short 2>/dev/null || echo '?')"
echo "  kubectl $(kubectl version --client -o json 2>/dev/null | sed -n 's/.*"gitVersion": *"\([^"]*\)".*/\1/p' | head -1)"
echo "  engine  $engine${KIND_EXPERIMENTAL_PROVIDER:+ (KIND_EXPERIMENTAL_PROVIDER=$KIND_EXPERIMENTAL_PROVIDER)}"
