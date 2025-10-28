# KEA DHCP REST API Client

This directory contains HTTP request files for testing the KEA DHCP REST API using VS Code REST Client extension.

## Setup

1. Install the [REST Client extension](https://marketplace.visualstudio.com/items?itemName=humao.rest-client) for VS Code
2. Ensure you have either `.env` or `.env-test` file in the project root

## How It Works

The HTTP request files use the `{{$dotenv VARIABLE_NAME}}` syntax to read values directly from `.env` files. This means:

- **No secrets in `.http` files** - They can be safely committed to git
- **No secrets in VS Code settings** - The `settings.json` file is clean
- **Environment switching** - Just use the appropriate `.env` file

## Quick Start

### For Local Development (with docker-compose)

Simply use the existing `.env` file. It should contain:

```env
KEA_URL=https://localhost:8000
KEA_TLS_CERT_FILE=./hack/docker/certs/client.crt
KEA_TLS_KEY_FILE=./hack/docker/certs/client.key
KEA_TLS_CA_FILE=./hack/docker/certs/ca.crt
```

Then in any `.http` file, click "Send Request" above the request with certificate auth (usually named with "Dev" suffix).

### For Test Environment

Copy `.env-test` to `.env`:

```bash
cp .env-test .env
```

The `.env-test` file contains:

```env
KEA_URL=https://tosl-k8s-dhcp01.test.mgmt.ld.nhn.no
KEA_PORT=8000
KEA_BASIC_AUTH_USERNAME=admin
KEA_BASIC_AUTH_PASSWORD=YourPassword
```

Then in any `.http` file, click "Send Request" above the request with basic auth (usually named with "Test" suffix).

## Switching Between Environments

**Option 1: Copy the file** (recommended)

```bash
# Switch to local dev
cp .env .env.current && echo "Using local dev"

# Switch to test
cp .env-test .env && echo "Using test environment"
```

**Option 2: Use symlinks**

```bash
# For local development
ln -sf .env .env.current

# For test environment
cp .env-test .env
```

**Option 3: Manual rename**

- Keep `.env-test` as is (git-ignored)
- Copy it to `.env` when you need to test against the test environment
- Restore your local `.env` when done

## Files

- `example.http` - Template showing the variable pattern
- `lease4-*.http` - Lease management requests
- `reservation-*.http` - Reservation management requests
- `subnet4-*.http` - Subnet management requests
- `config-write.http` - Configuration write request

## How Variables Work

Each `.http` file starts with variable definitions that read from `.env`:

```http
@baseUrl = {{$dotenv KEA_URL}}
@port = {{$dotenv KEA_PORT}}
@username = {{$dotenv KEA_BASIC_AUTH_USERNAME}}
@password = {{$dotenv KEA_BASIC_AUTH_PASSWORD}}
@certPath = {{$dotenv KEA_TLS_CERT_FILE}}
@keyPath = {{$dotenv KEA_TLS_KEY_FILE}}
@caPath = {{$dotenv KEA_TLS_CA_FILE}}
```

These variables are then used in the requests:

- Certificate auth: Uses `cert`, `key`, and `ca` headers
- Basic auth: Uses `Authorization: Basic {{username}} {{password}}`

## Security

✅ **Safe to commit:**

- All `.http` files (no secrets, only variable references)
- `example.http` (template file)
- This `README.md`

❌ **Never commit:**

- `.env` (already in `.gitignore`)
- `.env-test` (already in `.gitignore`)
- Any file with actual credentials

## Troubleshooting

**Variables not resolving?**

- Make sure you have a `.env` file in the project root (`/Users/rogerwesterbo/dev/github/viti/kea-operator/.env`)
- Check that the variable names in `.env` match what's referenced in the `.http` file
- The REST Client extension reads `.env` files from the workspace root

**Certificate errors?**

- Verify the certificate paths in `.env` are correct (relative to workspace root)
- For local dev, certificates should be in `hack/docker/certs/`

**Connection errors?**

- Verify the URL in your `.env` file is correct
- For local dev: `https://localhost:8000`
- For test: `https://tosl-k8s-dhcp01.test.mgmt.ld.nhn.no:8000`
