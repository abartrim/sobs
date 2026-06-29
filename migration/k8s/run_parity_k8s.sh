#!/usr/bin/env bash
# Fast byte-parity loop on the microk8s worker (node4, ci namespace) instead of flaky local colima.
#
# Syncs the LOCAL working tree (git-tracked + untracked-non-ignored files, so uncommitted WIP is
# included) into the long-lived `parity-dev` pod, then runs targeted parity for the given route ids.
# The pod has Python 3.14 + chdb 4.1.9 + Go 1.23 + the pinned amd64 libchdb already provisioned
# (see parity-devpod.yaml). Egress works in the `ci` namespace, so it behaves exactly like CI.
#
# Usage:
#   migration/k8s/run_parity_k8s.sh <comma-route-ids> [profile]
#   migration/k8s/run_parity_k8s.sh full          # run the whole gate (run_parity_ci.py)
#
# Examples:
#   migration/k8s/run_parity_k8s.sh get__api_web_traffic_geo__populated webtraffic
#   migration/k8s/run_parity_k8s.sh full
set -euo pipefail

KC="${KUBECONFIG_HP:-$HOME/.kube/hp.config}"
NS=ci
POD=parity-dev
K="kubectl --kubeconfig=$KC -n $NS"
REPO_ROOT="$(git rev-parse --show-toplevel)"

echo "[sync] streaming working tree -> $POD:/work/sobs"
cd "$REPO_ROOT"
# git-tracked + untracked-but-not-ignored => the source (excludes .git, node_modules, _run, goldens)
{ git ls-files; git ls-files --others --exclude-standard; } | sort -u \
  | tar -cf - -T - \
  | $K exec -i "$POD" -- bash -c 'cd /work/sobs && tar -xf - && echo "[sync] ok"'

MODE="${1:?usage: run_parity_k8s.sh <route-ids|full> [profile]}"

if [ "$MODE" = "full" ]; then
  echo "[gate] run_parity_ci.py (full byte-diff + LEDGER regen)"
  $K exec "$POD" -- bash -lc '
    export PATH=/usr/local/go/bin:$PATH CHDB_LIB_PATH=/work/.libchdb/libchdb.so
    cd /work/sobs && rm -rf migration/fixtures/_run && python migration/tools/run_parity_ci.py 2>&1 | tail -25'
  exit 0
fi

IDS="$MODE"
PROFILE="${2:-base}"
echo "[parity] ids=$IDS profile=$PROFILE"
$K exec "$POD" -- bash -lc "
  set -e
  export PATH=/usr/local/go/bin:\$PATH CHDB_LIB_PATH=/work/.libchdb/libchdb.so SOBS_PARITY_PORT=8799
  cd /work/sobs
  rm -rf migration/fixtures/_run
  python migration/tools/seed_fixtures.py >/dev/null
  if [ \"$PROFILE\" != base ]; then python migration/tools/seed_fixtures.py --only-profile $PROFILE >/dev/null || true; fi
  python migration/tools/capture_routes.py --only $IDS --profile $PROFILE 2>&1 | tail -3
  python migration/tools/seed_fixtures.py >/dev/null
  python migration/tools/parity_check.py --only $IDS 2>&1 | tail -12
"
