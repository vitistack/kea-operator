package settings

import (
	"github.com/spf13/viper"
	"github.com/vitistack/common/pkg/loggers/vlog"
	"github.com/vitistack/common/pkg/settings/dotenv"
	"github.com/vitistack/kea-operator/internal/consts"
)

func Init() {
	viper.SetDefault(consts.JSON_LOGGING, true)
	viper.SetDefault(consts.LOG_LEVEL, "info")
	viper.SetDefault(consts.KEA_DISABLE_KEEPALIVES, true)

	dotenv.LoadDotEnv()

	// Read environment variables automatically
	viper.AutomaticEnv()

	printEnvironmentSettings()
}

func printEnvironmentSettings() {
	settings := []string{
		consts.JSON_LOGGING,
		consts.LOG_LEVEL,
		consts.KEA_BASE_URL,
		consts.KEA_URL,
		consts.KEA_SECONDARY_URL,
		consts.KEA_PORT,
		consts.KEA_TLS_CA_FILE,
		consts.KEA_TLS_CERT_FILE,
		consts.KEA_TLS_KEY_FILE,
		consts.KEA_TLS_ENABLED,
		consts.KEA_TLS_INSECURE,
		consts.KEA_TLS_SERVER_NAME,
		consts.KEA_TIMEOUT_SECONDS,
		consts.KEA_TLS_SECRET_NAME,
		consts.KEA_TLS_SECRET_NAMESPACE,
		consts.KEA_DISABLE_KEEPALIVES,
	}

	for _, s := range settings {
		val := viper.Get(s)
		if val != nil {
			// #nosec G202
			vlog.Debug(s + "=" + viper.GetString(s))
		}
	}
}
