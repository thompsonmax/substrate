#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
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

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

# Measures actor resume latency on a kind cluster with ateapi's
# --inject-egress-trust-bundle off and on, per sandbox class, by running
# internal/e2e/bench/resume once per arm. The off arm is the plain install.
# The on arm is CI's MITM step: sdsmint egress, ate-api-server redeployed with
# the flag, and the counter demos recreated so their goldens carry the
# injected volume. Each arm is checked against the live ate-api-server args
# before it runs, so a stale cluster cannot be measured under the wrong label.

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-kind}"
export KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"

usage() {
  cat <<USAGE
Usage: $0 [options]

  --setup              Create the kind cluster and install substrate plus the
                       counter demos first (the e2e-test CI job's setup steps).
  --arm off|on|both    Which arm(s) to run (default: both). "on" flips the
                       cluster first unless --no-flip is given.
  --no-flip            The cluster is already on the on arm; do not redeploy.
  --classes LIST       Comma-separated sandbox classes (default: gvisor,microvm).
  --golden N           Fresh actors per class, each timed on its first resume
                       (default: 5).
  --cycles N           Pause/resume cycles per class on the first actor
                       (default: 20).
  --results DIR        Where samples.csv, phases.csv and summary.txt accumulate
                       (default: __resume-bench/<timestamp>, which git ignores).
  -h, --help           Show this help.

Prerequisites: docker, kind, kubectl, ko, go; /dev/kvm for the microvm class.
USAGE
}

ARM="both"
SETUP="false"
NO_FLIP="false"
CLASSES_CSV="gvisor,microvm"
GOLDEN=5
CYCLES=20
RESULTS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --setup) SETUP="true" ;;
    --arm) ARM="$2"; shift ;;
    --no-flip) NO_FLIP="true" ;;
    --classes) CLASSES_CSV="$2"; shift ;;
    --golden) GOLDEN="$2"; shift ;;
    --cycles) CYCLES="$2"; shift ;;
    --results) RESULTS="$2"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 1 ;;
  esac
  shift
done

case "${ARM}" in
  off|on|both) ;;
  *) echo "Error: --arm must be off, on or both" >&2; exit 1 ;;
esac

IFS=, read -r -a CLASSES <<< "${CLASSES_CSV}"
if [[ ${#CLASSES[@]} -eq 0 ]]; then
  echo "Error: --classes needs at least one of gvisor,microvm" >&2
  exit 1
fi
for class in "${CLASSES[@]}"; do
  case "${class}" in
    gvisor|microvm) ;;
    *) echo "Error: unknown sandbox class ${class}" >&2; exit 1 ;;
  esac
done

RESULTS="${RESULTS:-${ROOT}/__resume-bench/$(date +%Y%m%d-%H%M%S)}"
mkdir -p "${RESULTS}"

log() {
  echo
  echo "==> $*"
}

has_class() {
  local c
  for c in "${CLASSES[@]}"; do
    [[ "${c}" == "$1" ]] && return 0
  done
  return 1
}

api_server_args() {
  kubectl --context "${KUBECTL_CONTEXT}" -n ate-system get deployment ate-api-server \
    -o jsonpath='{.spec.template.spec.containers[0].args}'
}

# expect_arm fails before any sample is taken if the live ate-api-server
# disagrees with the arm about to be labeled.
expect_arm() {
  local args
  args="$(api_server_args)"
  case "$1" in
    on)
      if [[ "${args}" != *inject-egress-trust-bundle* ]]; then
        echo "Error: ate-api-server lacks --inject-egress-trust-bundle; drop --no-flip or redeploy with --experimental-use-sdsmint" >&2
        exit 1
      fi ;;
    off)
      if [[ "${args}" == *inject-egress-trust-bundle* ]]; then
        echo "Error: ate-api-server already runs --inject-egress-trust-bundle; the off arm needs a plain install" >&2
        exit 1
      fi ;;
  esac
  printf '%s\t%s\t%s\n' "$1" "$(date -u +%FT%TZ)" "${args}" >> "${RESULTS}/arms.tsv"
}

setup() {
  log "Creating the kind cluster and installing substrate"
  hack/create-kind-cluster.sh
  hack/install-ate-kind.sh --deploy-ate-system
  if has_class microvm; then
    hack/run-microvm-demo-kind.sh
  fi
  if has_class gvisor; then
    hack/install-ate-kind.sh --deploy-demo-counter
  fi
}

# flip_on is CI's "Deploy MITM egress (sdsmint)" and "Recreate counter demos
# under injection" steps, limited to the classes under test.
flip_on() {
  log "Switching to sdsmint egress and --inject-egress-trust-bundle"
  hack/install-ate-kind.sh --deploy-atenet --deploy-ate-apiserver --experimental-use-sdsmint
  local deploy=()
  if has_class gvisor; then
    kubectl --context "${KUBECTL_CONTEXT}" delete actortemplate counter -n ate-demo-counter --ignore-not-found
    deploy+=(--deploy-demo-counter)
  fi
  if has_class microvm; then
    kubectl --context "${KUBECTL_CONTEXT}" delete actortemplate counter-microvm -n ate-demo-counter-microvm --ignore-not-found
    deploy+=(--deploy-demo-counter-microvm)
  fi
  hack/install-ate-kind.sh "${deploy[@]}"
}

run_arm() {
  local label="$1" class
  expect_arm "${label}"
  for class in "${CLASSES[@]}"; do
    log "arm=${label} class=${class} golden=${GOLDEN} cycles=${CYCLES}"
    local envargs=(-u E2E_SANDBOX_CLASS
      BENCH_LABEL="${label}" BENCH_GOLDEN="${GOLDEN}" BENCH_CYCLES="${CYCLES}" BENCH_RESULTS_DIR="${RESULTS}")
    if [[ "${class}" == "microvm" ]]; then
      envargs+=(E2E_SANDBOX_CLASS=microvm)
    fi
    env "${envargs[@]}" hack/run-e2e-kind.sh ./internal/e2e/bench/resume \
      -run TestResumeLatency -count=1 -timeout 90m -args --no-color \
      2>&1 | tee "${RESULTS}/${label}-${class}.log"
  done
}

git rev-parse HEAD > "${RESULTS}/commit.txt"
if [[ "${SETUP}" == "true" ]]; then
  setup
fi

case "${ARM}" in
  off)
    run_arm off ;;
  on)
    if [[ "${NO_FLIP}" != "true" ]]; then
      flip_on
    fi
    run_arm on ;;
  both)
    run_arm off
    flip_on
    run_arm on ;;
esac

log "Results in ${RESULTS}"
cat "${RESULTS}/summary.txt"
