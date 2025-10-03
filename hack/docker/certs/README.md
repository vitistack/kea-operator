# Create certificates

# 1) Create a private CA (self-signed)

```bash
openssl genrsa -out ca.key 4096
openssl req -x509 -new -key ca.key -days 3650 -sha256 -subj "/CN=Kea-Local-CA" -out ca.crt
```

# 2) Create server key + CSR for the Kea Control Agent / HA endpoint

```bash
openssl genrsa -out server.key 4096
openssl req -new -key server.key -subj "/CN=kea.example.local" -out server.csr
```

# 3) Create a client key + CSR (for kea-shell or the peer)

```bash
openssl genrsa -out client.key 4096
openssl req -new -key client.key -subj "/CN=kea-client" -out client.csr
```

# 4) Sign both with your CA

```bash
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 825 -sha256

openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out client.crt -days 825 -sha256
```

# (optional) Convert client cert+key to a single PEM if you like

`cat client.crt client.key > client.full.pem`
