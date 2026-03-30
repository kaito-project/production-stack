#!/usr/bin/env bash

# Copyright (c) KAITO authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This script verifies that all Go source files have the required license header.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOILERPLATE="${REPO_ROOT}/hack/boilerplate.go.txt"

if [[ ! -f "${BOILERPLATE}" ]]; then
  echo "ERROR: boilerplate file not found at ${BOILERPLATE}"
  exit 1
fi

# Extract the first meaningful line from boilerplate to use as the check string.
CHECK_STRING="$(head -1 "${BOILERPLATE}")"

ERRORS=()

while IFS= read -r -d '' file; do
  # Skip vendor and generated files
  if [[ "${file}" == */vendor/* ]] || [[ "${file}" == */_output/* ]] || [[ "${file}" == */bin/* ]]; then
    continue
  fi
  # Accept either "// Copyright (c) KAITO" or "Copyright ... KAITO" (block comment style)
  if ! head -5 "${file}" | grep -qi "Copyright.*KAITO"; then
    ERRORS+=("${file}")
  fi
done < <(find "${REPO_ROOT}" -name "*.go" -not -path "*/vendor/*" -not -path "*/_output/*" -not -path "*/bin/*" -print0)

if [[ ${#ERRORS[@]} -gt 0 ]]; then
  echo "ERROR: The following Go files are missing the license header:"
  echo ""
  for f in "${ERRORS[@]}"; do
    echo "  ${f}"
  done
  echo ""
  echo "Please add the contents of hack/boilerplate.go.txt to the top of each file."
  exit 1
fi

echo "All Go files have the required license header."
