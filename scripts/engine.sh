#!/usr/bin/env bash
# Single source of truth for container-engine and compose detection.
#
# Sourced by the Makefile and by the other scripts:
#
#   . scripts/engine.sh
#   "$COMPOSE" "${PROFILE_FLAGS[@]}" up -d --build
#
# Exposes:
#   COMPOSE          — compose command as an argv prefix (may contain a space,
#                      e.g. "docker compose"); always expand unquoted.
#   CONTAINER_ENGINE — "podman" or "docker"
#
# Detection is deliberately NOT `docker compose version`, because on hosts where
# the docker CLI is a podman shim that command prints podman-compose's usage
# banner and still exits 0 — which would make us pick "docker compose" while
# actually running podman-compose. Instead we look at the real executables and
# only treat `docker compose` as Docker when a genuine Docker daemon answers.
#
# Override with WALLET_COMPOSE=... to force a specific implementation.

# shellcheck shell=bash

if [ -n "${WALLET_COMPOSE:-}" ]; then
  COMPOSE="$WALLET_COMPOSE"
  case "$COMPOSE" in
    podman*) CONTAINER_ENGINE=podman ;;
    *)       CONTAINER_ENGINE=docker ;;
  esac
else
  COMPOSE=""
  CONTAINER_ENGINE=""

  # 1. podman-compose, when podman itself is present.
  if command -v podman-compose >/dev/null 2>&1 && command -v podman >/dev/null 2>&1; then
    COMPOSE="podman-compose"
    CONTAINER_ENGINE="podman"
  # 2. A real Docker daemon behind `docker compose`.
  elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 \
       && docker compose version >/dev/null 2>&1; then
    COMPOSE="docker compose"
    CONTAINER_ENGINE="docker"
  # 3. Standalone docker-compose (v1 / standalone v2) with a real daemon.
  elif command -v docker-compose >/dev/null 2>&1 && command -v docker >/dev/null 2>&1 \
       && docker info >/dev/null 2>&1; then
    COMPOSE="docker-compose"
    CONTAINER_ENGINE="docker"
  # 4. Last resort: any podman-compose that at least parses.
  elif command -v podman-compose >/dev/null 2>&1; then
    COMPOSE="podman-compose"
    CONTAINER_ENGINE="podman"
  else
    echo "error: no usable compose implementation found" >&2
    echo "       install Docker (with a running daemon) or podman + podman-compose" >&2
    echo "       (override with WALLET_COMPOSE=... if you have something custom)" >&2
    return 1 2>/dev/null || exit 1
  fi
fi

export COMPOSE CONTAINER_ENGINE
