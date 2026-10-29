#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# bump-chart-deps.sh — Bump one dependency group of the productionstack
# umbrella chart (see hack/chart-deps.yaml) to the latest stable version in
# MCR, then refresh Chart.lock.
#
# Usage:
#   hack/bump-chart-deps.sh <group>
#
# Only stable X.Y.Z tags are considered, and versions are never downgraded.
# In GitHub Actions, writes `bumped`, `versions` and `summary` to
# $GITHUB_OUTPUT for the PR title and body.
#
# Requires: curl, jq, yq (v4), helm (v3.8+).
# ---------------------------------------------------------------------------
set -euo pipefail
shopt -s inherit_errexit

cd "$(dirname "${BASH_SOURCE[0]}")/.."
CONFIG=hack/chart-deps.yaml
export GROUP="${1:?Usage: $0 <group>}"
UMBRELLA="$(yq '.chart' "${CONFIG}")"
deps="$(yq '.groups[] | select(.name == strenv(GROUP)) | .dependencies[] | [.name, .image // ""] | @tsv' "${CONFIG}")"
[[ -n "${deps}" ]] || { echo "Group '${GROUP}' not found in ${CONFIG}" >&2; exit 1; }
summary=()
versions=()

# latest <host/repository> <current>: print the newest stable tag (same `v`
# prefix style as <current>) if it is newer than <current>.
latest() {
  local host="${1%%/*}" repo="${1#*/}" prefix="" tag
  [[ "$2" == v* ]] && prefix="v"
  tag="$(curl -fsS "https://${host}/v2/${repo}/tags/list?n=10000" | jq -r '.tags[]' |
    grep -E "^${prefix}[0-9]+\.[0-9]+\.[0-9]+$" | sort -V | tail -n1)"
  if [[ "$(printf '%s\n%s\n' "$2" "${tag}" | sort -V | tail -n1)" != "$2" ]]; then
    echo "${tag}"
  fi
}

# set_umbrella_dep <name> <version>
set_umbrella_dep() {
  sed -i "/^  - name: $1\$/,/version:/ s/version: .*/version: $2/" "${UMBRELLA}/Chart.yaml"
  export DEP="$1"
  [[ "$(yq '.dependencies[] | select(.name == strenv(DEP)) | .version' "${UMBRELLA}/Chart.yaml")" == "$2" ]] ||
    { echo "$1: failed to update ${UMBRELLA}/Chart.yaml" >&2; exit 1; }
}

# OCI chart: bump the umbrella dependency version.
bump_chart() {
  local name="$1" repo current new
  export DEP="${name}"
  repo="$(yq '.dependencies[] | select(.name == strenv(DEP)) | .repository' "${UMBRELLA}/Chart.yaml")"
  current="$(yq '.dependencies[] | select(.name == strenv(DEP)) | .version' "${UMBRELLA}/Chart.yaml")"
  new="$(latest "${repo#oci://}/${name}" "${current}")"
  if [[ -z "${new}" ]]; then
    echo "${name}: ${current} is up to date"
    return
  fi
  echo "${name}: ${current} -> ${new}"
  set_umbrella_dep "${name}" "${new}"
  summary+=("- \`${name}\` chart: \`${current}\` → \`${new}\`")
  versions+=("${new}")
}

# In-tree fork: the image tag, appVersion and README mentions all use the
# same tag, and the chart version mirrors it without the `v`.
bump_fork() {
  local name="$1" image="$2" dir="${UMBRELLA}/charts/$1" current new
  current="$(yq '.appVersion' "${dir}/Chart.yaml")"
  new="$(latest "${image}" "${current}")"
  if [[ -z "${new}" ]]; then
    echo "${name}: image ${current} is up to date"
    return
  fi
  echo "${name}: image ${current} -> ${new}"
  sed -i "s/\b${current//./\\.}\b/${new}/g" "${dir}/values.yaml" "${dir}/Chart.yaml" "${dir}/README.md"
  sed -i "s/^version: .*/version: ${new#v}/" "${dir}/Chart.yaml"
  set_umbrella_dep "${name}" "${new#v}"
  summary+=("- \`${name}\` image: \`${current}\` → \`${new}\`")
  versions+=("${new}")
}

while IFS=$'\t' read -r name image; do
  if [[ -n "${image}" ]]; then
    bump_fork "${name}" "${image}"
  else
    bump_chart "${name}"
  fi
done <<< "${deps}"

bumped=false
if [[ ${#summary[@]} -gt 0 ]]; then
  bumped=true
  helm dependency update "${UMBRELLA}"
fi
joined="$(printf '%s\n' "${versions[@]}" | sed '/^$/d' | sort -uV | paste -sd, - | sed 's/,/, /g')"
echo "bumped=${bumped} versions=${joined}"

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "bumped=${bumped}"
    echo "versions=${joined}"
    echo "summary<<__SUMMARY__"
    printf '%s\n' "${summary[@]}"
    echo "__SUMMARY__"
  } >> "${GITHUB_OUTPUT}"
fi
