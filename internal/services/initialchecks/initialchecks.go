package initialchecks

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/vitistack/common/pkg/loggers/vlog"
	"github.com/vitistack/kea-operator/internal/clients"
	"github.com/vitistack/kea-operator/internal/consts"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// InitialChecks verifies connectivity to Kea DHCP at startup using the configured client (Viper-driven).
// It attempts a lightweight command and fails fast if the service is unreachable after a few retries.
func InitialChecks() {
	host := viper.GetString(consts.KEA_HOST)
	port := viper.GetString(consts.KEA_PORT)
	base := viper.GetString(consts.KEA_BASE_URL)
	full := viper.GetString(consts.KEA_URL)

	if clients.KeaClient == nil {
		vlog.Error("Kea client not initialized; check configuration (KEA_HOST/PORT or KEA_URL)")
		os.Exit(1)
		return
	}

	// Retry a few times to tolerate slow startup/order
	const (
		maxRetries    = 3
		perTryTimeout = 5 * time.Second
		backoff       = 2 * time.Second
	)

	vlog.Info("checking connectivity to Kea", "url", nonEmpty(full, base), "host", host, "port", port)
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), perTryTimeout)
		err := pingKea(ctx)
		cancel()
		if err == nil {
			vlog.Info("kea connectivity OK")
			return
		}
		lastErr = err
		vlog.Warn("kea connectivity attempt failed", "attempt", attempt, "error", err)
		time.Sleep(backoff)
	}

	vlog.Error("failed to connect to Kea after retries", lastErr)
	os.Exit(1)
}

// pingKea sends a minimal command to verify reachability. We use 'list-commands' which is widely supported.
// If the server returns an 'unsupported' error text, it's still proof of reachability, so we treat it as success.
func pingKea(ctx context.Context) error {
	req := keamodels.Request{Command: "version-get"}
	resp, err := clients.KeaClient.Send(ctx, req)
	if err != nil {
		return err
	}
	if resp.Result == 0 {
		return nil
	}
	// Treat unsupported command as a connectivity success
	if resp.Text != "" {
		lower := strings.ToLower(resp.Text)
		if strings.Contains(lower, "unsupported") || strings.Contains(lower, "not supported") {
			return nil
		}
	}
	// Non-zero without unsupported => propagate an error to log but the caller will retry
	return fmt.Errorf("kea responded with non-success: %s", resp.Text)
}

func nonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
