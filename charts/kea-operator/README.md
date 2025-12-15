# kea-operator Helm Chart

A Helm chart for deploying the VitiStack KEA Operator, which manages DHCPv4 reservations in ISC Kea based on Kubernetes custom resources.

## Prerequisites

- Kubernetes 1.26+
- Helm 3.x
- VitiStack CRDs installed (NetworkConfiguration, NetworkNamespace)

## Installation

### Quick Install

```bash
helm install vitistack-kea-operator ./charts/kea-operator \
  --namespace vitistack \
  --create-namespace
```

### Install with KEA Configuration

```bash
helm install vitistack-kea-operator ./charts/kea-operator \
  --namespace vitistack \
  --create-namespace \
  --set kea.url="https://kea-dhcp.example.com:8000" \
  --set kea.auth.username="admin" \
  --set kea.auth.password="secret"
```

### Install with Existing Secret for Authentication

```bash
# Create secret first
kubectl create secret generic kea-credentials \
  --namespace vitistack \
  --from-literal=username=admin \
  --from-literal=password=secret

# Install with secret reference
helm install vitistack-kea-operator ./charts/kea-operator \
  --namespace vitistack \
  --set kea.url="https://kea-dhcp.example.com:8000" \
  --set kea.auth.existingSecret="kea-credentials" \
  --set kea.auth.usernameKey="username" \
  --set kea.auth.passwordKey="password"
```

### Install with TLS Configuration

```bash
helm install vitistack-kea-operator ./charts/kea-operator \
  --namespace vitistack \
  --create-namespace \
  --set kea.url="https://kea-dhcp.example.com:8000" \
  --set kea.tls.enabled="true" \
  --set kea.tls.secretName="kea-tls-certs" \
  --set kea.tls.secretNamespace="vitistack"
```

### Install from OCI Registry

```bash
helm install vitistack-kea-operator oci://ghcr.io/vitistack/charts/kea-operator \
  --namespace vitistack \
  --create-namespace \
  --set kea.url="https://kea-dhcp.example.com:8000"
```

## Uninstallation

```bash
helm uninstall vitistack-kea-operator --namespace vitistack
```

## Configuration

### General Settings

| Parameter          | Description                              | Default                          |
| ------------------ | ---------------------------------------- | -------------------------------- |
| `replicaCount`     | Number of replicas                       | `1`                              |
| `image.repository` | Image repository                         | `ghcr.io/vitistack/kea-operator` |
| `image.pullPolicy` | Image pull policy                        | `IfNotPresent`                   |
| `image.tag`        | Image tag (defaults to chart appVersion) | `""`                             |
| `imagePullSecrets` | Image pull secrets                       | `[]`                             |
| `nameOverride`     | Override chart name                      | `""`                             |
| `fullnameOverride` | Override full name                       | `""`                             |

### Service Account

| Parameter                    | Description                 | Default |
| ---------------------------- | --------------------------- | ------- |
| `serviceAccount.create`      | Create service account      | `true`  |
| `serviceAccount.automount`   | Automount API credentials   | `true`  |
| `serviceAccount.annotations` | Service account annotations | `{}`    |
| `serviceAccount.name`        | Service account name        | `""`    |

### KEA DHCP Configuration

| Parameter                  | Description                            | Default                                 |
| -------------------------- | -------------------------------------- | --------------------------------------- |
| `kea.url`                  | Primary KEA server URL                 | `""`                                    |
| `kea.secondaryUrl`         | Secondary KEA server URL (HA failover) | `""`                                    |
| `kea.port`                 | KEA server port                        | `"8000"`                                |
| `kea.timeoutSeconds`       | API request timeout                    | `"10"`                                  |
| `kea.disableKeepalives`    | Disable HTTP keep-alive                | `"true"`                                |
| `kea.requireClientClasses` | Required client classes for pools      | `"biosclients,ueficlients,ipxeclients"` |

### KEA Authentication

| Parameter                 | Description                | Default      |
| ------------------------- | -------------------------- | ------------ |
| `kea.auth.username`       | Basic auth username        | `""`         |
| `kea.auth.password`       | Basic auth password        | `""`         |
| `kea.auth.existingSecret` | Existing secret name       | `""`         |
| `kea.auth.usernameKey`    | Key in secret for username | `"username"` |
| `kea.auth.passwordKey`    | Key in secret for password | `"password"` |

### KEA TLS Configuration

| Parameter                 | Description                  | Default   |
| ------------------------- | ---------------------------- | --------- |
| `kea.tls.enabled`         | Enable TLS                   | `"false"` |
| `kea.tls.insecure`        | Skip TLS verification        | `"false"` |
| `kea.tls.serverName`      | TLS server name              | `""`      |
| `kea.tls.caFile`          | CA certificate file path     | `""`      |
| `kea.tls.certFile`        | Client certificate file path | `""`      |
| `kea.tls.keyFile`         | Client key file path         | `""`      |
| `kea.tls.secretName`      | TLS secret name              | `""`      |
| `kea.tls.secretNamespace` | TLS secret namespace         | `""`      |

### Logging Configuration

| Parameter                   | Description                          | Default   |
| --------------------------- | ------------------------------------ | --------- |
| `logging.level`             | Log level (debug, info, warn, error) | `"info"`  |
| `logging.jsonLogging`       | Enable JSON logging                  | `"true"`  |
| `logging.colorize`          | Colorize logs                        | `"false"` |
| `logging.addCaller`         | Add caller info to logs              | `"true"`  |
| `logging.disableStacktrace` | Disable stacktrace                   | `"false"` |
| `logging.unescapeMultiline` | Unescape multiline logs              | `"false"` |

### Probes

| Parameter                            | Description          | Default    |
| ------------------------------------ | -------------------- | ---------- |
| `livenessProbe.httpGet.path`         | Liveness probe path  | `/healthz` |
| `livenessProbe.httpGet.port`         | Liveness probe port  | `9995`     |
| `livenessProbe.initialDelaySeconds`  | Initial delay        | `15`       |
| `livenessProbe.periodSeconds`        | Check period         | `20`       |
| `readinessProbe.httpGet.path`        | Readiness probe path | `/readyz`  |
| `readinessProbe.httpGet.port`        | Readiness probe port | `9995`     |
| `readinessProbe.initialDelaySeconds` | Initial delay        | `5`        |
| `readinessProbe.periodSeconds`       | Check period         | `10`       |

### Resources

| Parameter                   | Description    | Default |
| --------------------------- | -------------- | ------- |
| `resources.limits.cpu`      | CPU limit      | `100m`  |
| `resources.limits.memory`   | Memory limit   | `128Mi` |
| `resources.requests.cpu`    | CPU request    | `100m`  |
| `resources.requests.memory` | Memory request | `128Mi` |

### Security Context

| Parameter                                  | Description                | Default |
| ------------------------------------------ | -------------------------- | ------- |
| `podSecurityContext.fsGroup`               | Pod filesystem group       | `65532` |
| `securityContext.allowPrivilegeEscalation` | Allow privilege escalation | `false` |
| `securityContext.readOnlyRootFilesystem`   | Read-only root filesystem  | `true`  |
| `securityContext.runAsNonRoot`             | Run as non-root            | `true`  |
| `securityContext.runAsUser`                | Run as user ID             | `65532` |
| `securityContext.runAsGroup`               | Run as group ID            | `65532` |

## Example values.yaml

```yaml
replicaCount: 1

image:
  repository: ghcr.io/vitistack/kea-operator
  pullPolicy: IfNotPresent
  tag: "latest"

kea:
  url: "https://kea-dhcp.vitistack.svc.cluster.local:8000"
  secondaryUrl: "https://kea-dhcp-secondary.vitistack.svc.cluster.local:8000"
  timeoutSeconds: "30"

  auth:
    existingSecret: "kea-credentials"
    usernameKey: "username"
    passwordKey: "password"

  tls:
    enabled: "true"
    secretName: "kea-tls"
    secretNamespace: "vitistack"

logging:
  level: "debug"
  jsonLogging: "true"

resources:
  limits:
    cpu: 200m
    memory: 256Mi
  requests:
    cpu: 100m
    memory: 128Mi
```

## Upgrading

```bash
helm upgrade vitistack-kea-operator ./charts/kea-operator \
  --namespace vitistack \
  --set kea.url="https://kea-dhcp.example.com:8000"
```

## RBAC

The chart creates the following RBAC resources:

- **ClusterRole**: Permissions for NetworkConfigurations, NetworkNamespaces, and Secrets
- **ClusterRoleBinding**: Binds the ClusterRole to the ServiceAccount
- **Role**: Leader election permissions (configmaps, leases, events)
- **RoleBinding**: Binds the leader election Role to the ServiceAccount

## Troubleshooting

### Check pod status

```bash
kubectl get pods -n vitistack -l app.kubernetes.io/name=kea-operator
```

### View logs

```bash
kubectl logs -n vitistack -l app.kubernetes.io/name=kea-operator -f
```

### Check health endpoints

```bash
kubectl port-forward -n vitistack svc/vitistack-kea-operator 9995:9995
curl http://localhost:9995/healthz
curl http://localhost:9995/readyz
```
