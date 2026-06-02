#!/usr/bin/env bash
set -euo pipefail

CR=${CR:-op-e2e}
BAD=${BAD:-op-invalid}
NS=${NS:-default}
API=${SMOLVM_API_URL:-http://127.0.0.1:8080}
SERVE_LOG=${SERVE_LOG:-/tmp/smolvm-serve-e2e.log}
SERVE_PID=${SERVE_PID:-/tmp/smolvm-serve-e2e.pid}
SMOLVM_BIN=${SMOLVM_BIN:-smolvm}

pass(){ echo "PASS: $*"; }
fail(){ echo "FAIL: $*" >&2; exit 1; }
phase(){ kubectl -n "$NS" get smolvm "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
machine(){ kubectl -n "$NS" get smolvm "$1" -o jsonpath='{.status.machineName}' 2>/dev/null || true; }
reason_line(){ kubectl -n "$NS" get smolvm "$1" -o jsonpath='{range .status.conditions[*]}{.type}:{.status}:{.reason}{" "}{end}' 2>/dev/null || true; }
api_state(){ curl -fsS "$API/api/v1/machines/$1" 2>/dev/null | jq -r .state 2>/dev/null || true; }
ensure_serve(){
  if ! curl -fsS "$API/health" >/dev/null 2>&1; then
    ($SMOLVM_BIN serve start --listen 0.0.0.0:8080 > "$SERVE_LOG" 2>&1 & echo $! > "$SERVE_PID")
    sleep 2
  fi
  curl -fsS "$API/health" >/dev/null || fail "smolvm serve is not reachable"
}
stop_serve(){
  if [ -f "$SERVE_PID" ]; then kill "$(cat "$SERVE_PID")" 2>/dev/null || true; fi
  pkill -f 'smolvm.*serve start --listen 0.0.0.0:8080' 2>/dev/null || true
  sleep 2
}
wait_phase(){
  local cr=$1 want=$2 timeout=${3:-120}
  local end=$((SECONDS+timeout))
  while [ $SECONDS -lt $end ]; do
    local p; p=$(phase "$cr")
    echo "wait $cr phase=$p want=$want reasons=$(reason_line "$cr")"
    [ "$p" = "$want" ] && return 0
    sleep 3
  done
  return 1
}
wait_api_state(){
  local m=$1 want=$2 timeout=${3:-120}
  local end=$((SECONDS+timeout))
  while [ $SECONDS -lt $end ]; do
    local s; s=$(api_state "$m")
    echo "wait machine $m state=$s want=$want"
    [ "$s" = "$want" ] && return 0
    sleep 3
  done
  return 1
}
wait_reason(){
  local cr=$1 want=$2 timeout=${3:-60}
  local end=$((SECONDS+timeout))
  while [ $SECONDS -lt $end ]; do
    local r; r=$(reason_line "$cr")
    echo "wait $cr reason contains $want: $r"
    grep -q "$want" <<<"$r" && return 0
    sleep 3
  done
  return 1
}

ensure_serve
kubectl delete smolvm "$CR" "$BAD" --ignore-not-found --wait=true --timeout=120s

cat <<YAML | kubectl apply -f -
apiVersion: vm.smolvm.dev/v1alpha1
kind: SmolVM
metadata:
  name: $CR
spec:
  running: true
  image: alpine:latest
  resources:
    cpus: 1
    memoryMiB: 512
  storage:
    storageGiB: 20
    overlayGiB: 10
  network:
    enabled: true
YAML
wait_phase "$CR" Running 180 || fail "create did not reach Running"
M=$(machine "$CR")
[ -n "$M" ] || fail "machine name missing"
wait_api_state "$M" running 30 || fail "machine not running"
pass "Create CR -> VM created/running ($M)"

kubectl patch smolvm "$CR" --type merge -p '{"spec":{"running":false}}'
wait_phase "$CR" Stopped 120 || fail "did not reach Stopped"
wait_api_state "$M" stopped 30 || fail "machine not stopped"
pass "spec.running=false -> VM stopped"

kubectl patch smolvm "$CR" --type merge -p '{"spec":{"running":true}}'
wait_phase "$CR" Running 180 || fail "did not restart"
wait_api_state "$M" running 30 || fail "machine not running"
pass "spec.running=true -> VM restarted"

cat <<YAML | kubectl apply -f -
apiVersion: vm.smolvm.dev/v1alpha1
kind: SmolVM
metadata:
  name: $BAD
spec:
  running: true
  image: alpine:latest
  from: /tmp/nope.smolmachine
YAML
wait_phase "$BAD" Failed 60 || fail "invalid spec did not fail"
wait_reason "$BAD" InvalidSpec 10 || fail "invalid spec reason missing"
pass "Invalid spec -> Failed condition"
kubectl delete smolvm "$BAD" --wait=true --timeout=60s

kubectl patch smolvm "$CR" --type merge -p '{"spec":{"storage":{"storageGiB":10,"overlayGiB":10}}}'
wait_reason "$CR" InvalidStorageResize 60 || fail "shrink did not report invalid resize"
pass "Storage shrink -> Invalid condition"
kubectl patch smolvm "$CR" --type merge -p '{"spec":{"storage":{"storageGiB":20,"overlayGiB":10}}}'
wait_phase "$CR" Running 120 || fail "recovery from shrink did not return Running"

stop_serve
kubectl annotate smolvm "$CR" e2e-api-down="$(date +%s)" --overwrite
wait_phase "$CR" Unknown 90 || fail "API unavailable did not become Unknown"
wait_reason "$CR" RuntimeUnavailable 10 || fail "RuntimeUnavailable reason missing"
pass "API unavailable -> Unknown/RuntimeUnavailable"
ensure_serve
wait_phase "$CR" Running 180 || fail "recovery after API restore failed"

kubectl -n operator-system rollout restart deploy/operator-controller-manager
kubectl -n operator-system rollout status deploy/operator-controller-manager --timeout=120s
wait_phase "$CR" Running 180 || fail "did not reconcile after operator restart"
pass "Operator restart -> status reconciles"

curl -fsS -X POST "$API/api/v1/machines/$M/stop" >/dev/null
wait_api_state "$M" stopped 30 || fail "manual stop failed"
wait_phase "$CR" Running 180 || fail "operator did not restart manually stopped machine"
wait_api_state "$M" running 30 || fail "machine not running after reconcile"
pass "manually stopped VM -> operator starts it again"

curl -fsS -X DELETE "$API/api/v1/machines/$M" >/dev/null
sleep 3
[ -z "$(api_state "$M")" ] || fail "manual API delete did not remove machine"
wait_phase "$CR" Running 180 || fail "operator did not recover after manual delete"
M2=$(machine "$CR")
[ "$M2" = "$M" ] || fail "machine name changed unexpectedly: $M2 != $M"
wait_api_state "$M" running 60 || fail "recreated machine not running"
pass "manually deleted VM -> operator recreates it"

kubectl delete smolvm "$CR" --wait=true --timeout=120s
if curl -fsS "$API/api/v1/machines/$M" >/dev/null 2>&1; then fail "machine still exists after CR delete"; fi
pass "Delete CR -> VM deleted"

pass "all MVP operator tests passed"
