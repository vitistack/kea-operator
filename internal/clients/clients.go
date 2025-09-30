package clients

import (
	"context"
	"os"

	"github.com/spf13/viper"
	"github.com/vitistack/kea-operator/internal/consts"
	"github.com/vitistack/kea-operator/pkg/clients/keaclient"
	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	// KeaClient is a singleton instance available to the operator.
	KeaClient keainterface.KeaClient
)

// InitializeClients initializes the global Kea client.
// It prefers environment variables (see internal/consts/consts.go) and
// falls back to in-cluster defaults if none are provided.
// Supports:
//   - HA: KEA_URL (primary) + KEA_SECONDARY_URL (optional)
//   - TLS (file or secret based)
//   - Basic Auth via KEA_BASIC_AUTH_USERNAME / KEA_BASIC_AUTH_PASSWORD (ignored if client certs provided)
func InitializeClients() {
	// Load environment variables
	viper.AutomaticEnv()
	_ = viper.BindEnv(consts.KEA_URL)
	_ = viper.BindEnv(consts.KEA_SECONDARY_URL)
	_ = viper.BindEnv(consts.KEA_PORT)
	_ = viper.BindEnv(consts.KEA_TLS_SECRET_NAME)
	_ = viper.BindEnv(consts.KEA_TLS_SECRET_NAMESPACE)
	_ = viper.BindEnv(consts.KEA_BASIC_AUTH_USERNAME)
	_ = viper.BindEnv(consts.KEA_BASIC_AUTH_PASSWORD)

	// Base options (env-based TLS, timeout, etc.)
	baseOpts := []keaclient.KeaOption{keaclient.OptionFromEnv()}

	// Attempt secret-based TLS if env specifies
	secretName := viper.GetString(consts.KEA_TLS_SECRET_NAME)
	secretNS := viper.GetString(consts.KEA_TLS_SECRET_NAMESPACE)
	if secretName != "" {
		// Default to POD namespace if none provided (K8s sets downward API value via fieldRef usually)
		if secretNS == "" {
			if nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
				secretNS = string(nsBytes)
			}
		}
		if cfg, err := config.GetConfig(); err == nil {
			kube, err2 := kubernetes.NewForConfig(cfg)
			if err2 == nil {
				if kc, err3 := BuildKeaClientFromSecret(context.Background(), kube, secretNS, secretName, baseOpts...); err3 == nil && kc != nil {
					KeaClient = kc
					return
				}
			}
		}
	}

	// If secret TLS wasn't used and basic auth username exists while no cert material was configured via env,
	// OptionFromEnv already populated the fields inside the client. We just construct now.
	KeaClient = keaclient.NewKeaClientWithOptions(baseOpts...)
}
