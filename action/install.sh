#!/usr/bin/env bash
# Installs the stratus binary into $1 and prints its path.
# STRATUS_VERSION: "latest", a release tag such as "v0.4.0", or "source".
set -euo pipefail
install_dir="$1"
mkdir -p "$install_dir"

exe=stratus
if [ "${RUNNER_OS:-}" = "Windows" ]; then exe=stratus.exe; fi

if [ "$STRATUS_VERSION" = "source" ]; then
  (cd "$ACTION_PATH" && go build -o "$install_dir/" ./cmd/stratus) >&2
else
  case "${RUNNER_OS}" in
    Linux) os=Linux; ext=tar.gz ;;
    macOS) os=Darwin; ext=tar.gz ;;
    Windows) os=Windows; ext=zip ;;
    *) echo "unsupported runner OS: ${RUNNER_OS}" >&2; exit 1 ;;
  esac
  case "${RUNNER_ARCH}" in
    X64) arch=x86_64 ;;
    ARM64) arch=arm64 ;;
    *) echo "unsupported runner arch: ${RUNNER_ARCH}" >&2; exit 1 ;;
  esac

  tag="$STRATUS_VERSION"
  if [ "$tag" = "latest" ]; then
    tag=$(curl -fsSL -H "Authorization: Bearer ${GH_TOKEN}" \
      https://api.github.com/repos/rmukubvu/stratus/releases/latest \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
    [ -n "$tag" ] || { echo "could not resolve the latest stratus release" >&2; exit 1; }
  fi
  version="${tag#v}"
  asset="stratus_${version}_${os}_${arch}.${ext}"
  url="https://github.com/rmukubvu/stratus/releases/download/${tag}/${asset}"
  echo "Downloading ${url}" >&2
  curl -fsSL -o "${RUNNER_TEMP}/${asset}" "$url"
  if [ "$ext" = "zip" ]; then
    unzip -o -q "${RUNNER_TEMP}/${asset}" -d "$install_dir"
  else
    tar -xzf "${RUNNER_TEMP}/${asset}" -C "$install_dir"
  fi
fi

"$install_dir/$exe" --help | head -n1 >&2
echo "$install_dir/$exe"
