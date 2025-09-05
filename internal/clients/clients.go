package clients

import (
	"github.com/spf13/viper"
	"github.com/vitistack/kea-operator/internal/consts"
	"github.com/vitistack/kea-operator/pkg/clients/keaclient"
	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
)

var (
	// KeaClient is a singleton instance available to the operator.
	KeaClient keainterface.KeaClient
)

// InitializeClients initializes the global Kea client.
// It prefers environment variables (see internal/consts/consts.go) and
// falls back to in-cluster defaults if none are provided.
func InitializeClients() {
	c := keaclient.NewKeaClientFromEnv()
	// Fallback to defaults if env not provided
	if c.BaseUrl == "" {
		c = keaclient.NewKeaClientWithOptions(
			keaclient.OptionHost(viper.GetString(consts.KEA_HOST)),
			keaclient.OptionPort(viper.GetString(consts.KEA_PORT)),
		)
	}
	KeaClient = c
}
