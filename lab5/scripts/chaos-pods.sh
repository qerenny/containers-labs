#!/usr/bin/env bash

set -euo pipefail

namespace="${NAMESPACE:-shop}"
interval_seconds="${CHAOS_INTERVAL_SECONDS:-5}"
duration_seconds="${CHAOS_DURATION_SECONDS:-240}"

if [[ ! "$interval_seconds" =~ ^[1-9][0-9]*$ ]]; then
  echo "CHAOS_INTERVAL_SECONDS must be a positive integer" >&2
  exit 1
fi

if [[ ! "$duration_seconds" =~ ^[1-9][0-9]*$ ]]; then
  echo "CHAOS_DURATION_SECONDS must be a positive integer" >&2
  exit 1
fi

deadline=$((SECONDS + duration_seconds))

echo "Starting pod chaos in namespace=$namespace for ${duration_seconds}s, interval=${interval_seconds}s"

while ((SECONDS < deadline)); do
  pod_names=$(kubectl get pods \
    --namespace "$namespace" \
    --selector 'app.kubernetes.io/part-of=shop,app.kubernetes.io/name in (api,worker)' \
    --field-selector status.phase=Running \
    --output go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}')

  if [[ -z "$pod_names" ]]; then
    echo "$(date --iso-8601=seconds) no non-terminating api/worker pods found; skipping this tick"
    sleep "$interval_seconds"
    continue
  fi

  victim=$(printf '%s\n' "$pod_names" | shuf --head-count=1)
  component=$(kubectl get pod "$victim" \
    --namespace "$namespace" \
    --output jsonpath='{.metadata.labels.app\.kubernetes\.io/name}')
  pod_ip=$(kubectl get pod "$victim" \
    --namespace "$namespace" \
    --output jsonpath='{.status.podIP}')
  api_ready=$(kubectl get deployment shop-api \
    --namespace "$namespace" \
    --output jsonpath='{.status.readyReplicas}')
  worker_ready=$(kubectl get deployment shop-worker \
    --namespace "$namespace" \
    --output jsonpath='{.status.readyReplicas}')
  api_endpoints=$(kubectl get endpointslice \
    --namespace "$namespace" \
    --selector kubernetes.io/service-name=shop-api \
    --output jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{" "}{end}')

  echo "$(date --iso-8601=seconds) deleting pod=$victim component=$component ip=$pod_ip ready=api:${api_ready:-0},worker:${worker_ready:-0} api_endpoints=[$api_endpoints]"

  kubectl delete pod "$victim" \
    --namespace "$namespace" \
    --wait=false

  sleep "$interval_seconds"
done

echo "Pod chaos finished"
