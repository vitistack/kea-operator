package consts

const (
	JSON_LOGGING = "JSON_LOGGING"
	LOG_LEVEL    = "LOG_LEVEL"

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
)
