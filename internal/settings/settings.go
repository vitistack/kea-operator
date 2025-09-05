package settings

import (
	"github.com/spf13/viper"
	"github.com/vitistack/kea-operator/internal/consts"
)

func Init() {
	viper.SetDefault(consts.JSON_LOGGING, true)
	viper.SetDefault(consts.LOG_LEVEL, "info")
	viper.SetDefault(consts.KEA_DISABLE_KEEPALIVES, true)

	// Read environment variables automatically
	viper.AutomaticEnv()
}
