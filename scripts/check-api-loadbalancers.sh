#!/usr/bin/env bash
#
# check-api-loadbalancers.sh
#
# Curl the Kubernetes API load balancer of every cluster and report whether it
# answers. The LB address is taken from each ControlPlaneVirtualSharedIP's
# status.loadBalancerIps; the API listens on 6443 and exposes an unauthenticated
# /healthz endpoint that returns "ok".
#
# Usage:
#   ./check-api-loadbalancers.sh [-n <namespace>] [-p <port>] [--path <path>]
#
#   -n, --namespace <ns>   Only check CPVIPs in this namespace (default: all)
#   -p, --port <port>      API port (default: 6443)
#       --path <path>      Endpoint to request (default: /healthz)
#       --pod <pod>        Run curl from this in-cluster pod (e.g. keacurl in
#                          'default') instead of locally. Useful when the LB IPs
#                          are only reachable from inside the cluster.
#       --pod-namespace <ns> Namespace of --pod (default: default)
#   -t, --timeout <secs>   Per-request timeout (default: 5)
#   -h, --help             Show this help
#
set -euo pipefail

NAMESPACE=""
PORT=6443
PATH_=/healthz
TIMEOUT=5
POD=""
POD_NS=default

while [[ $# -gt 0 ]]; do
	case "$1" in
		-n|--namespace) NAMESPACE="$2"; shift 2 ;;
		-p|--port) PORT="$2"; shift 2 ;;
		--path) PATH_="$2"; shift 2 ;;
		--pod) POD="$2"; shift 2 ;;
		--pod-namespace) POD_NS="$2"; shift 2 ;;
		-t|--timeout) TIMEOUT="$2"; shift 2 ;;
		-h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

ns_args=(-A)
[[ -n "$NAMESPACE" ]] && ns_args=(-n "$NAMESPACE")

# "namespace<TAB>name<TAB>lbip" per CPVIP that has a loadBalancerIp.
rows="$(kubectl get controlplanevirtualsharedips "${ns_args[@]}" -o json \
	| jq -r '.items[]
		| .metadata.namespace as $ns | .metadata.name as $n
		| (.status.loadBalancerIps // [])[]
		| "\($ns)\t\($n)\t\(.)"')"

if [[ -z "$rows" ]]; then
	echo "No ControlPlaneVirtualSharedIPs with a load balancer IP found." >&2
	exit 1
fi

# do_curl <url> : print HTTP code + body (first line), locally or via a pod.
do_curl() {
	local url="$1"
	if [[ -n "$POD" ]]; then
		kubectl exec "$POD" -n "$POD_NS" -- \
			sh -c 'curl -sS -k -m "$2" -o /dev/null -w "%{http_code}" "$1" 2>/dev/null || echo 000' \
			_ "$url" "$TIMEOUT"
	else
		curl -sS -k -m "$TIMEOUT" -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || echo 000
	fi
}

printf '%-28s %-20s %-18s %-6s %s\n' "CLUSTER" "NAMESPACE" "LB IP" "CODE" "RESULT"
printf '%-28s %-20s %-18s %-6s %s\n' \
	"----------------------------" "--------------------" "------------------" "----" "------"

ok=0; bad=0
unreachable=()
while IFS=$'\t' read -r ns name ip; do
	[[ -n "$ip" ]] || continue
	url="https://${ip}:${PORT}${PATH_}"
	code="$(do_curl "$url")"
	if [[ "$code" == "200" || "$code" == "401" || "$code" == "403" ]]; then
		result="reachable"; ok=$((ok + 1))
	else
		result="UNREACHABLE"; bad=$((bad + 1))
		unreachable+=("$name ($ns) $ip -> code $code")
	fi
	printf '%-28s %-20s %-18s %-6s %s\n' "$name" "$ns" "$ip" "$code" "$result"
done <<<"$rows"

echo
echo "Reachable: $ok   Unreachable: $bad"
if [[ "$bad" -gt 0 ]]; then
	echo
	echo "Unreachable clusters:"
	for u in "${unreachable[@]}"; do
		echo "  - $u"
	done
fi
[[ "$bad" -eq 0 ]]
