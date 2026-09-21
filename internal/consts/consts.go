package consts

const (
	DEVELOPMENT             = "DEVELOPMENT"
	LOG_JSON_LOGGING        = "LOG_JSON_LOGGING"
	LOG_LEVEL               = "LOG_LEVEL"
	LOG_COLORIZE            = "LOG_COLORIZE"
	LOG_ADD_CALLER          = "LOG_ADD_CALLER"
	LOG_DISABLE_STACKTRANCE = "LOG_DISABLE_STACKTRANCE"
	LOG_UNESCAPE_MULTILINE  = "LOG_UNESCAPE_MULTILINE"

	KEA_BASE_URL                = "KEA_BASE_URL"
	KEA_URL                     = "KEA_URL"            // full URL e.g. https://host:port (preferred)
	KEA_SECONDARY_URL           = "KEA_SECONDARY_URL"  // single backup URL (optional; kept for compatibility, see KEA_SECONDARY_URLS)
	KEA_SECONDARY_URLS          = "KEA_SECONDARY_URLS" // comma-separated backup URLs, tried in order once the primary gives up (optional)
	KEA_PORT                    = "KEA_PORT"
	KEA_TLS_CA_FILE             = "KEA_TLS_CA_FILE"
	KEA_TLS_CERT_FILE           = "KEA_TLS_CERT_FILE"
	KEA_TLS_KEY_FILE            = "KEA_TLS_KEY_FILE"
	KEA_TLS_ENABLED             = "KEA_TLS_ENABLED" // boolean toggle; default false
	KEA_TLS_INSECURE            = "KEA_TLS_INSECURE"
	KEA_TLS_SERVER_NAME         = "KEA_TLS_SERVER_NAME"
	KEA_TIMEOUT_SECONDS         = "KEA_TIMEOUT_SECONDS"         // whole-request timeout; default 60
	KEA_CONNECT_TIMEOUT_SECONDS = "KEA_CONNECT_TIMEOUT_SECONDS" // TCP connect + TLS handshake timeout; default 10
	KEA_TLS_SECRET_NAME         = "KEA_TLS_SECRET_NAME"         // #nosec G101
	KEA_TLS_SECRET_NAMESPACE    = "KEA_TLS_SECRET_NAMESPACE"    // #nosec G101
	KEA_DISABLE_KEEPALIVES      = "KEA_DISABLE_KEEPALIVES"      // boolean; disable HTTP keep-alive reuse

	// The primary Kea server is retried before any backup is used. A request
	// that times out, can't connect or gets HTTP 502/503/504 is retried
	// KEA_PRIMARY_RETRIES times (default 3), waiting KEA_RETRY_BACKOFF
	// (default 1s) doubling up to KEA_RETRY_MAX_BACKOFF (default 10s) between
	// attempts. After a failover the primary is skipped for
	// KEA_PRIMARY_COOLDOWN (default 30s; 0 = always try it first). Durations
	// are Go durations.
	KEA_PRIMARY_RETRIES   = "KEA_PRIMARY_RETRIES"
	KEA_RETRY_BACKOFF     = "KEA_RETRY_BACKOFF"
	KEA_RETRY_MAX_BACKOFF = "KEA_RETRY_MAX_BACKOFF"
	KEA_PRIMARY_COOLDOWN  = "KEA_PRIMARY_COOLDOWN"

	// Basic auth credentials (optional) – if set and no client certs provided, basic auth will be used
	KEA_BASIC_AUTH_USERNAME = "KEA_BASIC_AUTH_USERNAME"
	KEA_BASIC_AUTH_PASSWORD = "KEA_BASIC_AUTH_PASSWORD" // #nosec G101 false positive – variable name only

	// Pool configuration for subnet creation
	KEA_REQUIRE_CLIENT_CLASSES = "KEA_REQUIRE_CLIENT_CLASSES" // comma-separated list of client classes

	// KEA_STRICT_DEFAULTS, when true, makes the operator refuse to claim
	// NetworkConfigurations with an unset spec.provider or NetworkNamespaces
	// with a nil spec.ipAllocation. When false (default), the operator treats
	// unset values as the DHCP default for backward compatibility and logs a
	// deprecation notice once per resource.
	KEA_STRICT_DEFAULTS = "KEA_STRICT_DEFAULTS"

	// KEA_PIN_RESERVATIONS controls upgrading a MAC-only reservation to hold
	// the MAC's current lease IP: "off", "log" (default; report what would be
	// pinned, write nothing) or "enforce".
	KEA_PIN_RESERVATIONS = "KEA_PIN_RESERVATIONS"

	// KEA_CLEANUP_TIMEOUT is how long deletion retries a failed Kea reservation
	// cleanup before removing the finalizer anyway (Go duration; default 15m).
	KEA_CLEANUP_TIMEOUT = "KEA_CLEANUP_TIMEOUT"

	// MAX_CONCURRENT_RECONCILES is the maximum number of reconciliations run in
	// parallel per controller. The workqueue still serializes by object key, so
	// concurrency only applies across distinct objects. Defaults to 5 when unset.
	MAX_CONCURRENT_RECONCILES = "MAX_CONCURRENT_RECONCILES"
)
