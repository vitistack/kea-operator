#!/bin/bash
kubectl patch networknamespace test-networknamespace \
  --type=merge \
  --subresource=status \
  --patch '{
    "status": {
      "name": "test",
      "ipv4_prefix": "100.64.0.0/24",
      "ipv4_egress_ip": "100.64.0.1",
      "ipv6_prefix": "fd00:100:64::/64",
      "ipv6_egress_ip": "fd00:100:64::1",
      "vlan_id": 2101,
      "phase": "Ready",
      "status": "",
      "message": "",
      "associated_kubernetes_cluster_ids": ["cluster-1"]
    }
  }'
