# kea-operator

Kubernetes operator that ensures DHCPv4 reservations in ISC Kea based on VitiStack custom resources. It watches NetworkConfiguration objects, discovers the namespace IPv4 prefix via NetworkNamespace, resolves the matching Kea subnet, and for each MAC listed in the NetworkConfiguration ensures a reservation for the IP of its current lease.

## What it does

- Watches `vitistack.io/v1alpha1` NetworkConfiguration resources
- Reads MACs from `spec.networkInterfaces[].macAddress`
- Gets the namespace IPv4 prefix from `NetworkNamespace.status.ipv4Prefix`
- Resolves Kea subnet-id via `subnet4-list`
- Looks up current leases via `lease4-get-by-hw-address`
- Creates or confirms reservations with `reservation-add` (and removes on delete)

See also: docs/KEA-DHCP.md for running a local Kea server and REST quick tests.

## Quick start (local dev)

1. Start a local Kea DHCP4 with REST API enabled

- Requires Docker. This repo includes a compose file and sample config.
- On macOS you may need to allow unix socket dir:

```bash
chmod 750 ./hack/docker/sockets
docker-compose up -d
```

2. Point the operator to Kea

Set one of the following (ordered by precedence):

- `KEA_URL`: full URL, e.g. `http://localhost:8000`
- or `KEA_BASE_URL`/`KEA_HOST` and optional `KEA_PORT` (default 8000)
- `KEA_SECONDARY_URL` (optional): secondary URL for HA failover, e.g. `http://localhost:8001`

3. Run the controller locally

```bash
make run
```

## Deploy to a cluster

CRDs for NetworkConfiguration/NetworkNamespace live in vitistack/crds. Use the helper targets:

```bash
make install          # installs VitiStack CRDs into current kube-context
make deploy IMG=<your-repo>/viti-kea-operator:<tag>
```

Undeploy:

```bash
make undeploy
```

## Helm Installation

### Prerequisites

Create a Kubernetes secret with your Kea credentials:

```bash
kubectl create namespace vitistack

kubectl create secret generic kea-credentials \
  --namespace vitistack \
  --from-literal=username=your-kea-username \
  --from-literal=password=your-kea-password
```

### Install the Operator

```bash
helm install vitistack-kea-operator oci://ghcr.io/vitistack/helm/kea-operator \
  --namespace vitistack \
  --set kea.url="https://kea1.example.com" \
  --set kea.secondaryUrl="https://kea2.example.com" \
  --set kea.auth.existingSecret="kea-credentials"
```

For self-signed or internal CA certificates, add TLS insecure mode:

```bash
helm install vitistack-kea-operator oci://ghcr.io/vitistack/helm/kea-operator \
  --namespace vitistack \
  --set kea.url="https://kea1.example.com" \
  --set kea.secondaryUrl="https://kea2.example.com" \
  --set kea.tls.insecure=true \
  --set kea.auth.existingSecret="kea-credentials"
```

### Upgrade

```bash
helm upgrade vitistack-kea-operator oci://ghcr.io/vitistack/helm/kea-operator \
  --namespace vitistack \
  --reuse-values
```

### Uninstall

```bash
helm uninstall vitistack-kea-operator --namespace vitistack
```

For full Helm chart documentation, see [charts/kea-operator/README.md](charts/kea-operator/README.md).

## Usage example

Prereq: A NetworkNamespace with an IPv4 prefix in status (typically set by another controller):

```yaml
apiVersion: vitistack.io/v1alpha1
kind: NetworkNamespace
metadata:
	name: demo
status:
	ipv4Prefix: 10.123.0.0/24
```

Add a NetworkConfiguration listing MAC addresses. The operator will look up each MAC’s current lease in Kea and create a reservation for that IP in the namespace’s subnet. The CRD requires identifiers like `clusterName`, `datacenterName`, `supervisorName`, and `provider` in `spec`.

```yaml
apiVersion: vitistack.io/v1alpha1
kind: NetworkConfiguration
metadata:
	name: demo
	namespace: demo
spec:
  clusterName: example-cluster
  datacenterName: dc1
  supervisorName: demo
  provider: example
	networkInterfaces:
		- name: eth0
			macAddress: "00:02:12:34:56:78"
		- name: eth1
			macAddress: "1a:1b:1c:1d:1e:1f"
```

Notes

- MACs are normalized (case-insensitive; '-' allowed and normalized to ':').
- The operator requires the device to have already obtained a DHCP lease; otherwise it won’t create a reservation.
- On deletion of the NetworkConfiguration, reservations are best-effort removed.

## Configuration (env vars)

Kea client

- `KEA_URL` (preferred) full URL, e.g. `http://localhost:8000`
- `KEA_SECONDARY_URL` (optional) secondary URL for HA failover, e.g. `http://localhost:8001`
- `KEA_BASE_URL` or `KEA_HOST` + `KEA_PORT`
- `KEA_TIMEOUT_SECONDS` (default 10)
- `KEA_DISABLE_KEEPALIVES` (true/false)

Authentication

- Mutually exclusive options (basic auth is ignored if client certificate configured):
  - Basic auth: `KEA_BASIC_AUTH_USERNAME`, `KEA_BASIC_AUTH_PASSWORD`
  - mTLS client certificate: `KEA_TLS_CERT_FILE`, `KEA_TLS_KEY_FILE` (+ optional `KEA_TLS_CA_FILE`)

TLS (optional)

- `KEA_TLS_ENABLED` (true/false)
- `KEA_TLS_CA_FILE`, `KEA_TLS_CERT_FILE`, `KEA_TLS_KEY_FILE`
- `KEA_TLS_INSECURE` (true/false), `KEA_TLS_SERVER_NAME`

## Development

Helpful targets:

```bash
make help             # list all targets
make test             # unit tests (with envtest setup)
make build            # build manager binary
make lint             # golangci-lint
make go-security-scan # gosec static analysis
```

Repo layout highlights:

- `internal/controller/v1alpha1/` — reconciler for NetworkConfiguration
- `internal/services/kea/` — Kea service wrapper (subnet/lease/reservation calls)
- `pkg/clients/keaclient/` — HTTP client with TLS options and env var support
- `hack/docker/` — local Kea config, volumes, and certs

## Troubleshooting

- “unsupported kea command subnet4-list”: your Kea build/API may lack that command; the operator avoids hot loops and logs an error.
- “no lease found”: ensure the device obtained a lease; verify using the REST helpers under `hack/rest/` or curl the Kea API directly.
- Verify the operator can reach Kea at the URL/port you configured.

## Docs

- Kea DHCP setup and REST tips: https://github.com/vitistack/kea-operator/blob/main/docs/KEA-DHCP.md
