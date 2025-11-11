#!/bin/bash
kubectl patch networknamespace test-networknamespace -n test \
  --type=merge \
  --subresource=status \
  --patch '{
    "status": {
      "datacenterIdentifier": "test-south-az1",
      "supervisorIdentifier": "test-viti",
      "clusterIdentifier": "test-cluster",
      "ipv4Prefix": "10.123.0.0/24",
      "ipv4EgressIp": "10.123.0.1",
      "ipv6Prefix": "fd00:100:64::/64",
      "ipv6EgressIp": "fd00:100:64::1",
      "vlanId": 2101,
      "phase": "Ready",
      "status": "",
      "message": "",
      "associatedKubernetesClusterIds": ["cluster-1"]
    }
  }'
