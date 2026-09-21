package keaclient

import (
	"slices"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/vitistack/kea-operator/internal/consts"
)

// setEnv sets viper keys for one test and clears them afterwards.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		viper.Set(k, v)
		t.Cleanup(func() { viper.Set(k, nil) })
	}
}

// The legacy single KEA_SECONDARY_URL keeps working and goes first; the list
// is trimmed, de-duplicated and never contains the primary itself.
func TestOptionFromEnv_BackupList(t *testing.T) {
	setEnv(t, map[string]string{
		consts.KEA_URL:            "http://primary:8000",
		consts.KEA_SECONDARY_URL:  "http://a:8000",
		consts.KEA_SECONDARY_URLS: " http://b:8000, http://a:8000,,http://primary:8000 ,http://c:8000",
	})

	c := NewKeaClientWithOptions(OptionFromEnv())

	want := []string{"http://a:8000", "http://b:8000", "http://c:8000"}
	if !slices.Equal(c.SecondaryUrls, want) {
		t.Fatalf("backups = %q, want %q", c.SecondaryUrls, want)
	}
}

// retrySettings is what the retry options of a client came out as.
type retrySettings struct {
	retries                       int
	backoff, maxBackoff, cooldown time.Duration
}

func retrySettingsOf(c *keaClient) retrySettings {
	return retrySettings{c.PrimaryRetries, c.RetryBackoff, c.RetryMaxBackoff, c.PrimaryCooldown}
}

// Retry settings come from the environment; values that don't parse, or are
// negative, keep the default instead of silently becoming zero.
func TestOptionFromEnv_RetrySettings(t *testing.T) {
	defaults := retrySettingsOf(NewKeaClientWithOptions())
	tests := []struct {
		name string
		env  map[string]string
		want retrySettings
	}{
		{
			name: "all set",
			env: map[string]string{
				consts.KEA_PRIMARY_RETRIES:   "5",
				consts.KEA_RETRY_BACKOFF:     "250ms",
				consts.KEA_RETRY_MAX_BACKOFF: "2s",
				consts.KEA_PRIMARY_COOLDOWN:  "45s",
			},
			want: retrySettings{5, 250 * time.Millisecond, 2 * time.Second, 45 * time.Second},
		},
		{
			name: "zero disables retries and cooldown",
			env: map[string]string{
				consts.KEA_PRIMARY_RETRIES:  "0",
				consts.KEA_PRIMARY_COOLDOWN: "0s",
			},
			want: retrySettings{0, defaults.backoff, defaults.maxBackoff, 0},
		},
		{
			name: "invalid values keep defaults",
			env: map[string]string{
				consts.KEA_PRIMARY_RETRIES:   "lots",
				consts.KEA_RETRY_BACKOFF:     "soon",
				consts.KEA_RETRY_MAX_BACKOFF: "-1s",
				consts.KEA_PRIMARY_COOLDOWN:  "30",
			},
			want: defaults,
		},
		{
			name: "negative retries keep default",
			env:  map[string]string{consts.KEA_PRIMARY_RETRIES: "-2"},
			want: defaults,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			if got := retrySettingsOf(NewKeaClientWithOptions(OptionFromEnv())); got != tc.want {
				t.Fatalf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}
