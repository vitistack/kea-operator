#!/bin/bash
kubectl patch networkconfiguration test-networkconfiguration \
  --type=merge \
  --subresource=status \
  --patch '{
    "status": {
      "networkInterfaces": [
        {
          "name": "testeth1",
          "dhcpReserved": false,
          "dns": [],
          "ipv4Addresses": [],
          "ipv4Gateway": "",
          "ipv4Subnet": "",
          "macAddress": "00:02:12:34:56:78",
          "vlan": "",
          "ipv6Addresses": [],
          "ipv6Gateway": "",
          "ipv6Subnet": ""
        }
      ]
    }
  }'
