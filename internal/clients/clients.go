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
func InitializeClients() {
	// Base options (env first)
	baseOpts := []keaclient.KeaOption{keaclient.OptionFromEnv()}
	host := viper.GetString(consts.KEA_HOST)
	port := viper.GetString(consts.KEA_PORT)
	if host != "" {
		baseOpts = append(baseOpts, keaclient.OptionHost(host))
	}
	if port != "" {
		baseOpts = append(baseOpts, keaclient.OptionPort(port))
	}

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

	// Fallback: env/file based only
	KeaClient = keaclient.NewKeaClientWithOptions(baseOpts...)
}
