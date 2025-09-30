package initialchecks

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/vitistack/common/pkg/loggers/vlog"
	"github.com/vitistack/common/pkg/operator/crdcheck"
	"github.com/vitistack/kea-operator/internal/clients"
	"github.com/vitistack/kea-operator/internal/consts"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// InitialChecks verifies connectivity to Kea DHCP at startup using the configured client (Viper-driven).
// It attempts a lightweight command and fails fast if the service is unreachable after a few retries.
func InitialChecks() {
	if !checkKea() {
		os.Exit(1)
	}

	crdcheck.MustEnsureInstalled(context.TODO(),
		crdcheck.Ref{Group: "vitistack.io", Version: "v1alpha1", Resource: "networknamespaces"},     // your CRD plural
		crdcheck.Ref{Group: "vitistack.io", Version: "v1alpha1", Resource: "networkconfigurations"}, // your CRD plural
	)
}

func checkKea() bool {
	base := viper.GetString(consts.KEA_BASE_URL)
	full := viper.GetString(consts.KEA_URL)

	if clients.KeaClient == nil {
		vlog.Error("Kea client not initialized; check configuration (KEA_URL or KEA_BASE_URL)")
		os.Exit(1)
		return true
	}

	// Retry a few times to tolerate slow startup/order
	const (
		maxRetries    = 3
		perTryTimeout = 5 * time.Second
		backoff       = 2 * time.Second
	)

	vlog.Info("checking connectivity to Kea", "url", nonEmpty(full, base))
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), perTryTimeout)
		err := pingKea(ctx)
		cancel()
		if err == nil {
			vlog.Info("kea connectivity OK")
			return true
		}
		lastErr = err
		vlog.Warn("kea connectivity attempt failed ", "attempt: ", attempt, "error: ", err)
		time.Sleep(backoff)
	}

	vlog.Error("failed to connect to Kea after retries", lastErr)
	os.Exit(1)
	return false
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
