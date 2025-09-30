package keaclient

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/vitistack/kea-operator/internal/consts"
	corev1 "k8s.io/api/core/v1"
)

type KeaOption interface {
	apply(*keaClient)
}

type optionFunc func(*keaClient)

func (of optionFunc) apply(cfg *keaClient) { of(cfg) }

func OptionServerString(serverstring string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		var err error
		serverparts := strings.SplitN(serverstring, ":", 2)
		if len(serverparts) == 2 {
			cfg.BaseUrl = serverparts[0]
			cfg.Port = serverparts[1]
		} else {
			cfg.BaseUrl = serverparts[0]
			cfg.Port = "8000" // default KEA port
		}
		if cfg.BaseUrl == "" || cfg.Port == "" {
			fmt.Println("Error parsing server string: ", err)
		}
	})
}

func OptionHost(host string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.BaseUrl = host
	})
}

func OptionPort(port string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.Port = port
	})
}

// OptionURL sets a full URL (scheme://host:port). It parses out host and port and keeps scheme embedded in BaseUrl.
func OptionURL(fullURL string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		if fullURL == "" {
			return
		}
		// We just store as-is; buildBaseURL will not prepend scheme if already present.
		// If user supplies host separately we split host:port later.
		// Everything before last ':' is treated as the host portion if needed.
		cfg.BaseUrl = fullURL
		// Attempt to extract port if present at end
		// Only if scheme present and host:port provided.
		// This is optional; buildBaseURL already handles existing port, but storing port allows overrides via OptionPort.
		parts := strings.Split(fullURL, "://")
		if len(parts) == 2 {
			hostPort := parts[1]
			if slash := strings.Index(hostPort, "/"); slash >= 0 {
				hostPort = hostPort[:slash]
			}
			if hp := strings.Split(hostPort, ":"); len(hp) == 2 {
				cfg.Port = hp[1]
			}
		}
	})
}

// OptionSecondaryURL sets a secondary URL for HA failover.
func OptionSecondaryURL(fullURL string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		if fullURL == "" {
			return
		}
		cfg.SecondaryUrl = fullURL
	})
}

// TLS and HTTP options
func OptionTLS(caFile, certFile, keyFile string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.CACertPath = caFile
		cfg.ClientCertPath = certFile
		cfg.ClientKeyPath = keyFile
	})
}

func OptionInsecureSkipVerify(insecure bool) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.InsecureSkipVerify = insecure
	})
}

func OptionServerName(serverName string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.ServerName = serverName
	})
}

func OptionTimeout(d time.Duration) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.Timeout = d
		if cfg.HttpClient != nil {
			cfg.HttpClient.Timeout = d
		}
	})
}

// OptionFromEnv populates the client configuration from environment variables via Viper.
// Supported env vars (see consts):
//
//	KEA_URL (full URL with scheme, e.g. https://host:port) or KEA_BASE_URL + optional KEA_PORT
//	KEA_SECONDARY_URL (optional, for HA failover)
//	KEA_TLS_CA_FILE, KEA_TLS_CERT_FILE, KEA_TLS_KEY_FILE
//	KEA_TLS_INSECURE (true/false)
//	KEA_TLS_SERVER_NAME
//	KEA_TIMEOUT_SECONDS
func OptionFromEnv() KeaOption {
	return optionFunc(func(cfg *keaClient) {
		viper.AutomaticEnv()
		// Bind expected variables (ignore bind errors deliberately)
		_ = viper.BindEnv(consts.KEA_URL)
		_ = viper.BindEnv(consts.KEA_SECONDARY_URL)
		_ = viper.BindEnv(consts.KEA_BASE_URL)
		_ = viper.BindEnv(consts.KEA_PORT)
		_ = viper.BindEnv(consts.KEA_TLS_ENABLED)
		_ = viper.BindEnv(consts.KEA_TLS_CA_FILE)
		_ = viper.BindEnv(consts.KEA_TLS_CERT_FILE)
		_ = viper.BindEnv(consts.KEA_TLS_KEY_FILE)
		_ = viper.BindEnv(consts.KEA_TLS_INSECURE)
		_ = viper.BindEnv(consts.KEA_TLS_SERVER_NAME)
		_ = viper.BindEnv(consts.KEA_TIMEOUT_SECONDS)
		_ = viper.BindEnv(consts.KEA_DISABLE_KEEPALIVES)

		full := viper.GetString(consts.KEA_URL)
		secondary := viper.GetString(consts.KEA_SECONDARY_URL)
		base := viper.GetString(consts.KEA_BASE_URL)
		port := viper.GetString(consts.KEA_PORT)
		if full != "" {
			cfg.BaseUrl = full // includes scheme
		} else if base != "" {
			cfg.BaseUrl = base
		}
		if secondary != "" {
			cfg.SecondaryUrl = secondary
		}
		if port != "" {
			cfg.Port = port
		}
		// TLS settings only if enabled (default disabled)
		tlsEnabled := viper.GetBool(consts.KEA_TLS_ENABLED)
		if tlsEnabled {
			if v := viper.GetString(consts.KEA_TLS_CA_FILE); v != "" {
				cfg.CACertPath = v
			}
			if v := viper.GetString(consts.KEA_TLS_CERT_FILE); v != "" {
				cfg.ClientCertPath = v
			}
			if v := viper.GetString(consts.KEA_TLS_KEY_FILE); v != "" {
				cfg.ClientKeyPath = v
			}
			if viper.IsSet(consts.KEA_TLS_INSECURE) {
				cfg.InsecureSkipVerify = viper.GetBool(consts.KEA_TLS_INSECURE)
			}
			if v := viper.GetString(consts.KEA_TLS_SERVER_NAME); v != "" {
				cfg.ServerName = v
			}
		}
		if secs := viper.GetInt(consts.KEA_TIMEOUT_SECONDS); secs > 0 {
			cfg.Timeout = time.Duration(secs) * time.Second
			if cfg.HttpClient != nil {
				cfg.HttpClient.Timeout = cfg.Timeout
			}
		}
		if viper.GetBool(consts.KEA_DISABLE_KEEPALIVES) {
			cfg.disableKeepAlives = true
		}
	})
}

// OptionTLSPEM sets TLS data directly from in-memory PEM bytes (takes precedence over file paths).
func OptionTLSPEM(caPEM, certPEM, keyPEM []byte) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		if len(caPEM) > 0 {
			cfg.CACertPEM = caPEM
		}
		if len(certPEM) > 0 {
			cfg.ClientCertPEM = certPEM
		}
		if len(keyPEM) > 0 {
			cfg.ClientKeyPEM = keyPEM
		}
	})
}

// OptionTLSFromSecret populates TLS material from a Kubernetes Secret (type kubernetes.io/tls or generic keys).
// Expected keys:
//
//	ca.crt (optional), tls.crt, tls.key
func OptionTLSFromSecret(secret *corev1.Secret) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		if secret == nil {
			return
		}
		if ca, ok := secret.Data["ca.crt"]; ok {
			cfg.CACertPEM = ca
		}
		if crt, ok := secret.Data[corev1.TLSCertKey]; ok {
			cfg.ClientCertPEM = crt
		}
		if key, ok := secret.Data[corev1.TLSPrivateKeyKey]; ok {
			cfg.ClientKeyPEM = key
		}
	})
}
