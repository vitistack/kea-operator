package v1alpha1

import (
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/vitistack/kea-operator/internal/consts"
	keaservice "github.com/vitistack/kea-operator/internal/services/kea"
)

// An unknown pin mode must fall back to log, the mode that never writes.
func TestPinModeFromEnv(t *testing.T) {
	tests := []struct {
		raw  string
		want keaservice.PinMode
	}{
		{"off", keaservice.PinModeOff},
		{"log", keaservice.PinModeLog},
		{"enforce", keaservice.PinModeEnforce},
		{" Enforce ", keaservice.PinModeEnforce},
		{"", keaservice.PinModeLog},
		{"yes", keaservice.PinModeLog},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			viper.Set(consts.KEA_PIN_RESERVATIONS, tc.raw)
			t.Cleanup(func() { viper.Set(consts.KEA_PIN_RESERVATIONS, nil) })
			if got := pinModeFromEnv(); got != tc.want {
				t.Fatalf("pinModeFromEnv(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCleanupTimeoutFromEnv(t *testing.T) {
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"90s", 90 * time.Second},
		{"", 15 * time.Minute},
		{"soon", 15 * time.Minute},
		{"0s", 15 * time.Minute},
		{"-5m", 15 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			viper.Set(consts.KEA_CLEANUP_TIMEOUT, tc.raw)
			t.Cleanup(func() { viper.Set(consts.KEA_CLEANUP_TIMEOUT, nil) })
			if got := cleanupTimeoutFromEnv(); got != tc.want {
				t.Fatalf("cleanupTimeoutFromEnv(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
