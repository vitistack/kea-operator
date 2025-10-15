package consts

const (
	DEVELOPMENT             = "DEVELOPMENT"
	LOG_JSON_LOGGING        = "LOG_JSON_LOGGING"
	LOG_LEVEL               = "LOG_LEVEL"
	LOG_COLORIZE            = "LOG_COLORIZE"
	LOG_ADD_CALLER          = "LOG_ADD_CALLER"
	LOG_DISABLE_STACKTRANCE = "LOG_DISABLE_STACKTRANCE"
	LOG_UNESCAPE_MULTILINE  = "LOG_UNESCAPE_MULTILINE"

	KEA_BASE_URL             = "KEA_BASE_URL"
	KEA_URL                  = "KEA_URL"           // full URL e.g. https://host:port (preferred)
	KEA_SECONDARY_URL        = "KEA_SECONDARY_URL" // secondary URL for HA failover (optional)
	KEA_PORT                 = "KEA_PORT"
	KEA_TLS_CA_FILE          = "KEA_TLS_CA_FILE"
	KEA_TLS_CERT_FILE        = "KEA_TLS_CERT_FILE"
	KEA_TLS_KEY_FILE         = "KEA_TLS_KEY_FILE"
	KEA_TLS_ENABLED          = "KEA_TLS_ENABLED" // boolean toggle; default false
	KEA_TLS_INSECURE         = "KEA_TLS_INSECURE"
	KEA_TLS_SERVER_NAME      = "KEA_TLS_SERVER_NAME"
	KEA_TIMEOUT_SECONDS      = "KEA_TIMEOUT_SECONDS"
	KEA_TLS_SECRET_NAME      = "KEA_TLS_SECRET_NAME"      // #nosec G101
	KEA_TLS_SECRET_NAMESPACE = "KEA_TLS_SECRET_NAMESPACE" // #nosec G101
	KEA_DISABLE_KEEPALIVES   = "KEA_DISABLE_KEEPALIVES"   // boolean; disable HTTP keep-alive reuse
	// Basic auth credentials (optional) – if set and no client certs provided, basic auth will be used
	KEA_BASIC_AUTH_USERNAME = "KEA_BASIC_AUTH_USERNAME"
	KEA_BASIC_AUTH_PASSWORD = "KEA_BASIC_AUTH_PASSWORD" // #nosec G101 false positive – variable name only
)
