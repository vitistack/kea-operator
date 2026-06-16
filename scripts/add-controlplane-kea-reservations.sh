#!/usr/bin/env bash
#
# add-controlplane-kea-reservations.sh
#
# Purpose
#   Control-plane nodes of a Talos KubernetesCluster are not getting the correct
#   DHCP addresses. This script pins them by creating Kea DHCP host reservations
#   that map each control-plane node's MAC address to the IP the platform expects
#   it to have.
#
# What it does
#   1. Looks up the KubernetesCluster by name and reads spec.data.clusterId.
#   2. Lists the cluster's control-plane Machines
#      (labels: vitistack.io/clusterid=<clusterId>, vitistack.io/node-role=control-plane)
#      and extracts the MAC address from the interface whose status type == "domain".
#   3. Reads the IPs from the ControlPlaneVirtualSharedIP CR (.spec.poolMembers).
#   4. Pairs them by index (Nth control-plane machine, sorted by name -> Nth poolMember IP).
#   5. Resolves the Kea subnet-id for each IP via `subnet4-list`.
#   6. Writes the reservations to Kea: first deletes any existing reservation for
#      the MAC in every configured subnet, then reservation-add (idempotent),
#      running every curl call from inside an ephemeral curlimages/curl pod.
#
# All Kea API calls run inside a curl pod in the cluster, because the Kea Control
# Agent is only reachable from within the cluster network.
#
# Requirements (on the workstation running this script):
#   - kubectl (with access to the cluster holding the CRDs AND able to run the curl pod)
#   - jq
#   - awk, base64
#
# Credentials:
#   The Kea Control Agent uses HTTP Basic auth. Provide credentials either via:
#     - env vars KEA_USERNAME and KEA_PASSWORD, or
#     - a Kubernetes Secret (KEA_CREDENTIALS_SECRET) with keys username/password.
#   Credentials are written into the curl pod via stdin as a curl --netrc file, so
#   they never appear on a command line (argv) or in the pod spec.
#
# Usage:
#   ./add-controlplane-kea-reservations.sh [<kubernetesClusterName>] [-n <namespace>] [options]
#
#   If <kubernetesClusterName> is omitted, every KubernetesCluster in the target
#   namespace is processed.
#
# Options:
#   -n, --namespace <ns>     Namespace of the KubernetesCluster / Machines / CPVIP
#                            (default: current kubectl context namespace, else "default")
#   -y, --yes                Do not prompt for confirmation before writing to Kea
#       --dry-run            Print the planned reservations and payloads; do not call Kea
#       --existing-pod <p>   Reuse an already-running curl pod instead of creating one.
#                            The pod is not created or deleted by this script.
#       --username <u>       Kea Basic-auth username (overrides env/secret)
#       --password-stdin     Read the Kea Basic-auth password from stdin (recommended;
#                            avoids shell history-expansion issues with '!' etc.)
#   -h, --help               Show this help
#
# Environment variables (with defaults):
#   KEA_URL                  Kea Control Agent URL
#                            (default: https://mtrd-k8s-dhcp01.mgmt.ld.nhn.no:8000)
#   KEA_USERNAME             Basic-auth username (overrides the secret)
#   KEA_PASSWORD             Basic-auth password (overrides the secret)
#   KEA_CREDENTIALS_SECRET   Secret holding credentials (default: kea-api-credentials)
#   KEA_SECRET_NAMESPACE     Namespace of that secret (default: helper pod namespace)
#   KEA_SECRET_USER_KEY      Secret key for the username (default: username)
#   KEA_SECRET_PASS_KEY      Secret key for the password (default: password)
#   KEA_INSECURE             "true" to skip TLS verification with curl -k (default: true)
#   KEA_SERVICE              Kea service to target (default: dhcp4)
#   KEA_OPERATION_TARGET     reservation operation-target (default: all)
#   HELPER_NAMESPACE         Namespace to run the curl pod in (default: default)
#   CURL_IMAGE               Image for the curl pod (default: curlimages/curl:8.11.0)
#   EXISTING_POD             Reuse this already-running pod (same as --existing-pod)
#
set -euo pipefail

# --------------------------------------------------------------------------- #
# Defaults / configuration
# --------------------------------------------------------------------------- #
KEA_URL="${KEA_URL:-https://mtrd-k8s-dhcp01.mgmt.ld.nhn.no:8000}"
KEA_CREDENTIALS_SECRET="${KEA_CREDENTIALS_SECRET:-kea-api-credentials}"
KEA_SECRET_USER_KEY="${KEA_SECRET_USER_KEY:-username}"
KEA_SECRET_PASS_KEY="${KEA_SECRET_PASS_KEY:-password}"
KEA_INSECURE="${KEA_INSECURE:-true}"
KEA_SERVICE="${KEA_SERVICE:-dhcp4}"
KEA_OPERATION_TARGET="${KEA_OPERATION_TARGET:-all}"
HELPER_NAMESPACE="${HELPER_NAMESPACE:-default}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.11.0}"
KEA_SECRET_NAMESPACE="${KEA_SECRET_NAMESPACE:-$HELPER_NAMESPACE}"
EXISTING_POD="${EXISTING_POD:-}"

CP_ROLE_LABEL="vitistack.io/node-role"
CP_ROLE_VALUE="control-plane"
CLUSTER_ID_LABEL="vitistack.io/clusterid"

ASSUME_YES=false
DRY_RUN=false
NAMESPACE=""
CLUSTER_NAME=""
PASSWORD_STDIN=false

# --------------------------------------------------------------------------- #
# Logging helpers
# --------------------------------------------------------------------------- #
log()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
err()  { printf '[%s] ERROR: %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die()  { err "$*"; exit 1; }

usage() {
	sed -n '2,70p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

# --------------------------------------------------------------------------- #
# Argument parsing
# --------------------------------------------------------------------------- #
while [[ $# -gt 0 ]]; do
	case "$1" in
		-n|--namespace) NAMESPACE="${2:-}"; shift 2 ;;
		-y|--yes)       ASSUME_YES=true; shift ;;
		--dry-run)      DRY_RUN=true; shift ;;
		--existing-pod) EXISTING_POD="${2:-}"; shift 2 ;;
		--username)     KEA_USERNAME="${2:-}"; shift 2 ;;
		--password-stdin) PASSWORD_STDIN=true; shift ;;
		-h|--help)      usage 0 ;;
		-*)             die "unknown option: $1 (use --help)" ;;
		*)
			if [[ -z "$CLUSTER_NAME" ]]; then
				CLUSTER_NAME="$1"; shift
			else
				die "unexpected extra argument: $1"
			fi
			;;
	esac
done

[[ -n "$CLUSTER_NAME" || -n "$NAMESPACE" ]] || true  # cluster name is optional; namespace resolved below

# --------------------------------------------------------------------------- #
# Dependency checks
# --------------------------------------------------------------------------- #
for bin in kubectl jq awk base64; do
	command -v "$bin" >/dev/null 2>&1 || die "required command not found: $bin"
done

# Resolve namespace: explicit flag, else current context namespace, else "default".
if [[ -z "$NAMESPACE" ]]; then
	NAMESPACE="$(kubectl config view --minify -o jsonpath='{..namespace}' 2>/dev/null || true)"
	NAMESPACE="${NAMESPACE:-default}"
fi

# --------------------------------------------------------------------------- #
# Determine target cluster(s)
#   - explicit <kubernetesClusterName> -> just that one
#   - omitted                          -> every KubernetesCluster in the namespace
# --------------------------------------------------------------------------- #
declare -a CLUSTERS=()
if [[ -n "$CLUSTER_NAME" ]]; then
	CLUSTERS=("$CLUSTER_NAME")
else
	log "No cluster name given; discovering all KubernetesClusters in namespace '$NAMESPACE'..."
	while IFS= read -r c; do
		[[ -n "$c" ]] && CLUSTERS+=("$c")
	done < <(kubectl get kubernetescluster -n "$NAMESPACE" \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
	[[ "${#CLUSTERS[@]}" -gt 0 ]] || die "no KubernetesClusters found in namespace '$NAMESPACE'"
	log "Found ${#CLUSTERS[@]} cluster(s): ${CLUSTERS[*]}"
fi
SINGLE_CLUSTER=false
[[ -n "$CLUSTER_NAME" ]] && SINGLE_CLUSTER=true

# --------------------------------------------------------------------------- #
# gather_cluster <name> : resolve clusterId, the control-plane MACs (from the
# 'domain' interface) and the CPVIP poolMember IPs for one KubernetesCluster,
# validate the index pairing, and append the result to the global PLAN_* arrays.
# Returns 1 (logging an error) if the cluster cannot be planned; the caller
# decides whether that is fatal.
# --------------------------------------------------------------------------- #
declare -a PLAN_CLUSTER=() PLAN_NAME=() PLAN_MAC=() PLAN_IP=() PLAN_CURIP=()

gather_cluster() {
	local cluster_name="$1"
	local cluster_json cluster_id machines_json mac_lines cpvip_json
	local -a names=() macs=() ips=() curips=()

	cluster_json="$(kubectl get kubernetescluster "$cluster_name" -n "$NAMESPACE" -o json 2>/dev/null)" \
		|| { err "[$cluster_name] KubernetesCluster not found in namespace '$NAMESPACE'"; return 1; }
	cluster_id="$(jq -r '.spec.data.clusterId // empty' <<<"$cluster_json")"
	[[ -n "$cluster_id" ]] || { err "[$cluster_name] has no spec.data.clusterId"; return 1; }

	machines_json="$(kubectl get machines -n "$NAMESPACE" \
		-l "${CLUSTER_ID_LABEL}=${cluster_id},${CP_ROLE_LABEL}=${CP_ROLE_VALUE}" \
		-o json 2>/dev/null)" || { err "[$cluster_name] failed to list Machines"; return 1; }

	# "machineName<TAB>mac<TAB>currentIPv4" per control-plane machine, sorted by
	# name. The MAC is the first interface reporting "domain" as one of its
	# (comma-joined) status types, since KubeVirt sets .type from the VMI
	# InfoSource. currentIPv4 is that same interface's current IPv4 address (if
	# any), used to skip nodes that already have the expected address.
	mac_lines="$(jq -r '
		.items
		| sort_by(.metadata.name)[]
		| . as $m
		| ( ($m.status.networkInterfaces // [])
		    | map(select(((.type // "")
		                  | split(",")
		                  | map(gsub("^\\s+|\\s+$"; ""))
		                  | index("domain")) != null))
		    | .[0] ) as $domain
		| ($domain.macAddress // "") as $mac
		| ( ($domain.ipAddresses // [])
		    | map(select(test("^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$")))
		    | (.[0] // "") ) as $curip
		| "\($m.metadata.name)\t\($mac)\t\($curip)"
	' <<<"$machines_json")"
	[[ -n "$mac_lines" ]] || { err "[$cluster_name] no control-plane Machines found (clusterId '$cluster_id')"; return 1; }

	while IFS=$'\t' read -r name mac curip; do
		[[ -n "$name" ]] || continue
		if [[ -z "$mac" ]]; then
			err "[$cluster_name] control-plane Machine '$name' has no 'domain' interface (cannot determine MAC); skipping cluster"
			return 1
		fi
		names+=("$name")
		macs+=("$(printf '%s' "$mac" | tr 'A-Z' 'a-z')")
		curips+=("$curip")
	done <<<"$mac_lines"

	cpvip_json="$(kubectl get controlplanevirtualsharedips "$cluster_id" -n "$NAMESPACE" -o json 2>/dev/null)" \
		|| { err "[$cluster_name] ControlPlaneVirtualSharedIP '$cluster_id' not found in namespace '$NAMESPACE'"; return 1; }
	while IFS= read -r ip; do
		[[ -n "$ip" ]] && ips+=("$ip")
	done < <(jq -r '.spec.poolMembers // [] | .[]' <<<"$cpvip_json")
	[[ "${#ips[@]}" -gt 0 ]] || { err "[$cluster_name] CPVIP '$cluster_id' has no spec.poolMembers"; return 1; }

	if [[ "${#macs[@]}" -ne "${#ips[@]}" ]]; then
		err "[$cluster_name] count mismatch: ${#macs[@]} control-plane MAC(s) vs ${#ips[@]} poolMember IP(s); skipping cluster"
		err "[$cluster_name] machines: ${names[*]}"
		err "[$cluster_name] poolMembers: ${ips[*]}"
		return 1
	fi

	local i
	for i in "${!macs[@]}"; do
		PLAN_CLUSTER+=("$cluster_name")
		PLAN_NAME+=("${names[$i]}")
		PLAN_MAC+=("${macs[$i]}")
		PLAN_IP+=("${ips[$i]}")
		PLAN_CURIP+=("${curips[$i]}")
	done
	log "[$cluster_name] planned ${#macs[@]} reservation(s) (clusterId $cluster_id)"
	return 0
}

# --------------------------------------------------------------------------- #
# Build the combined reservation plan across all target clusters.
# --------------------------------------------------------------------------- #
skipped_clusters=0
for cl in "${CLUSTERS[@]}"; do
	gather_cluster "$cl" || skipped_clusters=$((skipped_clusters + 1))
done

if [[ "$SINGLE_CLUSTER" == "true" && "$skipped_clusters" -gt 0 ]]; then
	die "could not build a reservation plan for cluster '$CLUSTER_NAME'"
fi
[[ "${#PLAN_MAC[@]}" -gt 0 ]] || die "no reservations to apply (all clusters skipped)"
[[ "$skipped_clusters" -eq 0 ]] || err "skipped $skipped_clusters cluster(s) due to errors above (continuing with the rest)."

log "Planned reservations (paired by index):"
printf '\n  %-18s %-28s %-20s %-16s %-16s\n' "CLUSTER" "MACHINE" "MAC (type=domain)" "EXPECTED IP" "CURRENT IP" >&2
printf '  %-18s %-28s %-20s %-16s %-16s\n' \
	"------------------" "----------------------------" "--------------------" "----------------" "----------------" >&2
for i in "${!PLAN_MAC[@]}"; do
	cur="${PLAN_CURIP[$i]:-"-"}"
	[[ "$cur" == "${PLAN_IP[$i]}" ]] && cur="$cur (ok)"
	printf '  %-18s %-28s %-20s %-16s %-16s\n' \
		"${PLAN_CLUSTER[$i]}" "${PLAN_NAME[$i]}" "${PLAN_MAC[$i]}" "${PLAN_IP[$i]}" "$cur" >&2
done
printf '\n' >&2

# --------------------------------------------------------------------------- #
# Resolve Kea Basic-auth credentials.
# Order: explicit env/flags -> Secret -> interactive prompt.
# Skipped entirely for --dry-run, which never contacts Kea.
# --------------------------------------------------------------------------- #
KEA_USERNAME="${KEA_USERNAME:-}"
KEA_PASSWORD="${KEA_PASSWORD:-}"

if [[ "$DRY_RUN" != "true" ]]; then
	# 1. Password from stdin if requested (no argv/history exposure).
	if [[ "$PASSWORD_STDIN" == "true" ]]; then
		IFS= read -r KEA_PASSWORD || true
	fi

	# 2. If still missing, try the Secret (only when a username is also missing,
	#    or the secret carries both).
	if [[ -z "$KEA_USERNAME" || -z "$KEA_PASSWORD" ]]; then
		if secret_json="$(kubectl get secret "$KEA_CREDENTIALS_SECRET" -n "$KEA_SECRET_NAMESPACE" -o json 2>/dev/null)"; then
			log "Reading Kea credentials from Secret '$KEA_CREDENTIALS_SECRET' (namespace '$KEA_SECRET_NAMESPACE')..."
			s_user="$(jq -r --arg k "$KEA_SECRET_USER_KEY" '.data[$k] // empty' <<<"$secret_json" | base64 -d 2>/dev/null || true)"
			s_pass="$(jq -r --arg k "$KEA_SECRET_PASS_KEY" '.data[$k] // empty' <<<"$secret_json" | base64 -d 2>/dev/null || true)"
			[[ -z "$KEA_USERNAME" ]] && KEA_USERNAME="$s_user"
			[[ -z "$KEA_PASSWORD" ]] && KEA_PASSWORD="$s_pass"
		fi
	fi

	# 3. Interactive prompt for whatever is still missing. read -rs keeps the
	#    password out of the terminal echo, argv, and shell history (no '!' issues).
	if [[ -z "$KEA_USERNAME" ]]; then
		printf 'Kea username: ' >&2
		IFS= read -r KEA_USERNAME || true
	fi
	if [[ -z "$KEA_PASSWORD" ]]; then
		printf 'Kea password (input hidden): ' >&2
		IFS= read -rs KEA_PASSWORD || true
		printf '\n' >&2
	fi

	[[ -n "$KEA_USERNAME" && -n "$KEA_PASSWORD" ]] \
		|| die "no Kea credentials: provide --username/--password-stdin, KEA_USERNAME/KEA_PASSWORD, or Secret '$KEA_CREDENTIALS_SECRET'"
fi

# TLS verification: -k (insecure) by default, since the Kea Control Agent
# typically presents a self-signed/internal certificate.
INSECURE_FLAG=""
[[ "$KEA_INSECURE" == "true" ]] && INSECURE_FLAG="-k"

# --------------------------------------------------------------------------- #
# Confirmation gate (writing to shared DHCP infrastructure)
# --------------------------------------------------------------------------- #
if [[ "$DRY_RUN" != "true" && "$ASSUME_YES" != "true" ]]; then
	printf 'About to write %d reservation(s) across %d cluster(s) to Kea at %s. Continue? [y/N] ' \
		"${#PLAN_MAC[@]}" "${#CLUSTERS[@]}" "$KEA_URL" >&2
	read -r reply
	case "$reply" in
		y|Y|yes|YES) ;;
		*) die "aborted by user" ;;
	esac
fi

# --------------------------------------------------------------------------- #
# JSON payload builders (Kea Control Agent command format)
# --------------------------------------------------------------------------- #
build_subnet_list() {
	jq -nc --arg svc "$KEA_SERVICE" \
		'{command:"subnet4-list", service:[$svc]}'
}

build_reservation_del() {
	local subnet_id="$1" mac="$2"
	jq -nc \
		--arg svc "$KEA_SERVICE" \
		--argjson sid "$subnet_id" \
		--arg mac "$mac" \
		--arg target "$KEA_OPERATION_TARGET" \
		'{command:"reservation-del", service:[$svc],
		  arguments:{"subnet-id":$sid, "identifier-type":"hw-address",
		             "identifier":$mac, "operation-target":$target}}'
}

build_reservation_add() {
	local subnet_id="$1" mac="$2" ip="$3"
	jq -nc \
		--arg svc "$KEA_SERVICE" \
		--argjson sid "$subnet_id" \
		--arg mac "$mac" \
		--arg ip "$ip" \
		--arg target "$KEA_OPERATION_TARGET" \
		'{command:"reservation-add", service:[$svc],
		  arguments:{"operation-target":$target,
		             reservation:{"subnet-id":$sid, "hw-address":$mac, "ip-address":$ip}}}'
}

# --------------------------------------------------------------------------- #
# Dry-run: print payloads and exit before touching the cluster/Kea.
# --------------------------------------------------------------------------- #
if [[ "$DRY_RUN" == "true" ]]; then
	log "DRY-RUN: subnet lookup + reservation payloads (subnet-id 0 is a placeholder, resolved at runtime):"
	printf '  POST %s\n  %s\n\n' "$KEA_URL" "$(build_subnet_list)" >&2
	for i in "${!PLAN_MAC[@]}"; do
		printf '  POST %s  # delete MAC reservation in EVERY subnet, then add for %s/%s\n  %s\n  %s\n\n' \
			"$KEA_URL" "${PLAN_CLUSTER[$i]}" "${PLAN_NAME[$i]}" \
			"$(build_reservation_del 0 "${PLAN_MAC[$i]}")" \
			"$(build_reservation_add 0 "${PLAN_MAC[$i]}" "${PLAN_IP[$i]}")" >&2
	done
	log "DRY-RUN complete. No changes made."
	exit 0
fi

# --------------------------------------------------------------------------- #
# Curl helper pod lifecycle
# --------------------------------------------------------------------------- #
if [[ -n "$EXISTING_POD" ]]; then
	# Reuse an already-running pod. We neither create nor delete it.
	HELPER_POD="$EXISTING_POD"
	log "Reusing existing curl pod '$HELPER_POD' in namespace '$HELPER_NAMESPACE'..."
	kubectl get pod "$HELPER_POD" -n "$HELPER_NAMESPACE" >/dev/null 2>&1 \
		|| die "existing pod '$HELPER_POD' not found in namespace '$HELPER_NAMESPACE'"
	kubectl wait --for=condition=Ready "pod/$HELPER_POD" -n "$HELPER_NAMESPACE" --timeout=30s >/dev/null \
		|| die "existing pod '$HELPER_POD' is not Ready"
else
	HELPER_POD="kea-cp-reservation-$$-${RANDOM}"

	cleanup() {
		kubectl delete pod "$HELPER_POD" -n "$HELPER_NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	}
	trap cleanup EXIT

	log "Starting curl helper pod '$HELPER_POD' in namespace '$HELPER_NAMESPACE'..."
	# securityContext keeps the pod compliant with PodSecurity "restricted".
	pod_overrides="$(jq -nc --arg name "$HELPER_POD" --arg image "$CURL_IMAGE" '{
	  spec: {
	    securityContext: { runAsNonRoot: true, seccompProfile: { type: "RuntimeDefault" } },
	    containers: [{
	      name: $name,
	      image: $image,
	      command: ["sleep","3600"],
	      securityContext: {
	        allowPrivilegeEscalation: false,
	        runAsNonRoot: true,
	        capabilities: { drop: ["ALL"] },
	        seccompProfile: { type: "RuntimeDefault" }
	      }
	    }]
	  }
	}')"
	kubectl run "$HELPER_POD" -n "$HELPER_NAMESPACE" \
		--image="$CURL_IMAGE" --restart=Never \
		--overrides="$pod_overrides" \
		--command -- sleep 3600 >/dev/null

	kubectl wait --for=condition=Ready "pod/$HELPER_POD" -n "$HELPER_NAMESPACE" --timeout=90s >/dev/null \
		|| die "curl helper pod did not become Ready"
fi

# Write a curl config into the pod so credentials never appear in plaintext on a
# command line. NOTE: `kubectl exec -i` stdin is unreliable against this image
# (it intermittently delivers 0 bytes), so we pass the credential base64-encoded
# as an argument and decode it inside the pod via a local pipe. `user = "u:p"`
# makes curl send HTTP Basic auth preemptively (the Kea Control Agent issues no
# 401 challenge, so lazy/netrc auth fails).
curlrc_b64="$(printf 'user = "%s:%s"\n' "$KEA_USERNAME" "$KEA_PASSWORD" | base64 | tr -d '\n')"
kubectl exec "$HELPER_POD" -n "$HELPER_NAMESPACE" -- \
	sh -c 'umask 077; printf "%s" "$1" | base64 -d > /tmp/.curlrc' _ "$curlrc_b64" \
	|| die "failed to write Kea credentials into pod '$HELPER_POD'"
unset curlrc_b64

# Verify the credential file was actually written and is non-empty, otherwise
# every Kea call would silently fail with 401.
curlrc_size="$(kubectl exec "$HELPER_POD" -n "$HELPER_NAMESPACE" -- sh -c 'wc -c < /tmp/.curlrc' 2>/dev/null | tr -d '[:space:]')"
[[ "${curlrc_size:-0}" -gt 0 ]] \
	|| die "failed to write Kea credentials into pod '$HELPER_POD' (curlrc is empty)"

# kea_call <json-body> : POST the body to Kea from inside the pod, echo raw response.
# The body is passed as an argument (it is not secret) and piped to curl inside
# the pod, avoiding the unreliable `kubectl exec -i` stdin path.
kea_call() {
	local body="$1"
	kubectl exec "$HELPER_POD" -n "$HELPER_NAMESPACE" -- \
		sh -c 'printf "%s" "$3" | curl -sS $1 -K /tmp/.curlrc -H "Content-Type: application/json" -X POST "$2" --data-binary @-' \
		_ "$INSECURE_FLAG" "$KEA_URL" "$body"
}

# kea_first <response-json> : normalise Kea responses to a single object.
# Successful commands return a JSON array [ {...} ]; auth/agent errors return a
# bare object { "result": N, "text": "..." }. Handle both.
kea_first() {
	jq -c 'if type == "array" then (.[0] // {}) else . end' <<<"$1" 2>/dev/null || echo '{}'
}

# kea_result <response-json> : echo "<result-code>\t<text>"
kea_result() {
	local first; first="$(kea_first "$1")"
	jq -r '((.result // 1) | tostring) + "\t" + (.text // "")' <<<"$first" 2>/dev/null \
		|| printf '1\tunparseable response: %s' "$1"
}

# --------------------------------------------------------------------------- #
# 5. Resolve subnet-id for each IP via subnet4-list
# --------------------------------------------------------------------------- #
log "Querying Kea subnet4-list to resolve subnet-id(s)..."
subnet_resp="$(kea_call "$(build_subnet_list)")"
subnet_first="$(kea_first "$subnet_resp")"
subnet_rc="$(jq -r '.result // 1' <<<"$subnet_first" 2>/dev/null || echo 1)"
[[ "$subnet_rc" == "0" ]] || die "subnet4-list failed: $(kea_result "$subnet_resp")"

# Lines of "id<TAB>cidr" for every configured subnet.
SUBNETS_TSV="$(jq -r '.arguments.subnets // [] | .[] | "\(.id)\t\(.subnet)"' <<<"$subnet_first")"
[[ -n "$SUBNETS_TSV" ]] || die "Kea returned no subnets from subnet4-list"

# subnet_id_for_ip <ip> : echo the subnet id whose CIDR contains <ip>, or empty.
subnet_id_for_ip() {
	local ip="$1"
	awk -v ip="$ip" '
		function ip2int(a,  p){ split(a, p, "."); return ((p[1]*256+p[2])*256+p[3])*256+p[4] }
		BEGIN { ti = ip2int(ip) }
		{
			n = split($2, c, "/"); if (n != 2) next
			net = ip2int(c[1]); pfx = c[2] + 0
			if (pfx < 0 || pfx > 32) next
			block = 2 ^ (32 - pfx)
			if (int(ti / block) * block == int(net / block) * block) { print $1; exit }
		}
	' <<<"$SUBNETS_TSV"
}

# --------------------------------------------------------------------------- #
# 6. Apply reservations (del then add) for each MAC/IP pair
# --------------------------------------------------------------------------- #
failures=0
skipped=0
for i in "${!PLAN_MAC[@]}"; do
	cluster="${PLAN_CLUSTER[$i]}"
	name="${PLAN_NAME[$i]}"
	mac="${PLAN_MAC[$i]}"
	ip="${PLAN_IP[$i]}"
	curip="${PLAN_CURIP[$i]}"
	label="$cluster/$name"

	# If the Machine already reports the expected IPv4 on its 'domain' interface,
	# the node is correct -- leave its Kea reservation untouched.
	if [[ "$curip" == "$ip" ]]; then
		log "[$label] already has correct IP $ip; skipping"
		skipped=$((skipped + 1))
		continue
	fi

	sid="$(subnet_id_for_ip "$ip")"
	if [[ -z "$sid" ]]; then
		err "[$label] no Kea subnet contains IP $ip; skipping"
		failures=$((failures + 1))
		continue
	fi
	log "[$label] mac=$mac ip=$ip subnet-id=$sid"

	# Delete any existing reservation(s) for this MAC FIRST, across every
	# configured subnet (not just the target one). A node that got the wrong
	# DHCP address may have a stale reservation pinned in another subnet; if we
	# only cleared the target subnet, that stale entry would keep winning.
	# reservation-del is identifier-based and requires a subnet-id, so we issue
	# one delete per subnet -- but we pipe them all into a SINGLE `kubectl exec`
	# and loop inside the pod, otherwise the per-exec overhead (~0.4s) times the
	# number of subnets makes this very slow.
	del_bodies=""
	while IFS=$'\t' read -r del_sid _cidr; do
		[[ -n "$del_sid" ]] || continue
		del_bodies+="$(build_reservation_del "$del_sid" "$mac")"$'\n'
	done <<<"$SUBNETS_TSV"

	# Pass the bodies as an argument (not via exec stdin, which is unreliable for
	# this image) and loop over them inside the pod with a local pipe.
	kubectl exec "$HELPER_POD" -n "$HELPER_NAMESPACE" -- \
		sh -c 'ins="$1"; url="$2"; printf "%s\n" "$3" | while IFS= read -r body; do
				[ -n "$body" ] || continue
				curl -sS $ins -K /tmp/.curlrc -H "Content-Type: application/json" \
					-X POST "$url" --data-binary "$body" >/dev/null 2>&1 || true
			done' _ "$INSECURE_FLAG" "$KEA_URL" "$del_bodies" || true
	log "[$label] cleared any existing reservation for $mac (all subnets)"

	# reservation-add with the correct IP.
	add_resp="$(kea_call "$(build_reservation_add "$sid" "$mac" "$ip")")" || true
	add_line="$(kea_result "$add_resp")"
	add_rc="${add_line%%$'\t'*}"
	add_text="${add_line#*$'\t'}"
	if [[ "$add_rc" == "0" ]]; then
		log "[$label] OK: reserved $ip for $mac in subnet $sid"
	else
		err "[$label] reservation-add failed (result=$add_rc): $add_text"
		failures=$((failures + 1))
	fi
done

# --------------------------------------------------------------------------- #
# Summary
# --------------------------------------------------------------------------- #
total="${#PLAN_MAC[@]}"
applied=$((total - failures - skipped))
log "Done. ${applied}/${total} reservation(s) applied successfully across ${#CLUSTERS[@]} cluster(s)."
[[ "$skipped" -gt 0 ]] && log "${skipped} node(s) already had the correct IP and were skipped."
if [[ "$failures" -gt 0 ]]; then
	err "${failures} reservation(s) failed."
	exit 1
fi

log "Note: nodes must renew their DHCP lease (or reboot) to pick up the reserved IP."
