package initialchecks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vitistack/kea-operator/internal/clients"
	"github.com/vitistack/kea-operator/pkg/clients/keaclient"
)

// With the default retry settings, a primary that refuses connections is
// retried for several seconds before the client fails over. The startup check
// must wait for that and accept the backup's answer, rather than give up on
// its own deadline and exit the operator while a backup is serving.
func TestWaitForKea_ReachesBackupWhenPrimaryRefuses(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	primary := dead.URL
	dead.Close() // connections to the primary are now refused

	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"result":0,"text":"1.0.0"}]`))
	}))
	defer backup.Close()

	prev := clients.KeaClient
	clients.KeaClient = keaclient.NewKeaClientWithOptions(keaclient.OptionURL(primary), keaclient.OptionSecondaryURL(backup.URL))
	t.Cleanup(func() { clients.KeaClient = prev })

	if err := waitForKea(); err != nil {
		t.Fatalf("expected the startup check to reach the backup, got %v", err)
	}
}
