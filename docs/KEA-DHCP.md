# Kea DHCP Operator Documentation

![Kea DHCP](https://img.shields.io/badge/Kea-DHCP-blue?style=for-the-badge)
![Status](https://img.shields.io/badge/status-active-brightgreen?style=for-the-badge)
![License](https://img.shields.io/badge/license-MIT-blue?style=for-the-badge)

---

## 🚀 Overview

Kea DHCP Operator provides automated management and orchestration for [Kea DHCP](https://kea.isc.org/) servers in containerized environments. This documentation covers setup, configuration, and advanced usage.

---

## 📦 Features

- Automated deployment of Kea DHCP
- Pre-configured DHCP scope with options for PXE boot
- TLS certificate support
- Easy integration with Kubernetes and Docker for testing out REST-API

---

## 🛠️ Quick Start

```bash
git clone https://github.com/vitistack/kea-operator.git
cd kea-operator
chmod 750 ./hack/docker/sockets
docker-compose up -d
```

---

## ⚙️ Configuration

Configuration files are located in `hack/docker/config/`. Example:

```json
{
	"Dhcp4": {
		"interfaces-config": { "interfaces": [ "eth0" ] },
		"lease-database": { "type": "memfile", "persist": true },
		"subnet4": [ { "subnet": "192.168.1.0/24", "pools": [ { "pool": "192.168.1.10-192.168.1.100" } ] } ]
	}
}
```

---

## 📚 Resources

- [Kea DHCP Official Docs](https://kea.readthedocs.io/en/latest/)
- [Kea Operator GitHub](https://github.com/vitistack/kea-operator)

---

## 📝 License

This project is licensed under the MIT License.
