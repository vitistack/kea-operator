package keaclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// Bodies that parse as JSON but carry no Kea "result" must not be reported as
// success. Before, a null body, an empty object, or an auth/proxy page shaped
// as JSON decoded into a zero-valued Response — i.e. result 0.
func TestParseResponse_RejectsBodiesWithoutResult(t *testing.T) {
	c := &keaClient{}
	for _, body := range []string{
		`null`,
		`{}`,
		`[]`,
		`[{}]`,
		`{"text":"Unauthorized"}`,
		`[{"text":"no result here"}]`,
		`{"responses":[]}`,
		`"just a string"`,
		`<html>502 Bad Gateway</html>`,
	} {
		t.Run(body, func(t *testing.T) {
			resp, err := c.parseResponse([]byte(body))
			if err == nil {
				t.Fatalf("expected an error for body %s, got response %+v", body, resp)
			}
		})
	}
}

// Every shape Kea actually answers with must keep parsing: the Control Agent's
// array, the DHCP daemon's single object (Kea 3 direct HTTP socket), the
// wrapped form, and arguments that are not an object (list-commands).
func TestParseResponse_AcceptsKeaShapes(t *testing.T) {
	c := &keaClient{}
	tests := []struct {
		name       string
		body       string
		wantResult int
		wantText   string
		wantArg    string // key expected in Arguments, "" = expect nil Arguments
	}{
		{"control agent array", `[{"result":0,"text":"ok","arguments":{"leases":[]}}]`, 0, "ok", "leases"},
		{"single object", `{"result":3,"text":"0 IPv4 lease(s) found."}`, 3, "0 IPv4 lease(s) found.", ""},
		{"wrapped responses", `{"responses":[{"result":1,"text":"bad"}]}`, 1, "bad", ""},
		{"array arguments", `[{"result":0,"arguments":["subnet4-list","lease4-get"]}]`, 0, "", ""},
		{"single object with arguments", `{"result":0,"arguments":{"subnets":[]}}`, 0, "", "subnets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := c.parseResponse([]byte(tc.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Result != tc.wantResult || resp.Text != tc.wantText {
				t.Fatalf("got result=%d text=%q, want result=%d text=%q", resp.Result, resp.Text, tc.wantResult, tc.wantText)
			}
			if tc.wantArg == "" {
				if resp.Arguments != nil {
					t.Fatalf("expected nil arguments, got %v", resp.Arguments)
				}
				return
			}
			if _, ok := resp.Arguments[tc.wantArg]; !ok {
				t.Fatalf("expected argument %q in %v", tc.wantArg, resp.Arguments)
			}
		})
	}
}

// A non-2xx HTTP answer is a failure even if its body happens to look like a
// successful Kea response (e.g. a proxy or auth layer echoing JSON).
func TestSend_NonSuccessHTTPStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`[{"result":0,"text":"ok"}]`))
	}))
	defer srv.Close()

	c := NewKeaClientWithOptions(OptionURL(srv.URL))
	if _, err := c.Send(context.Background(), keamodels.Request{Command: "version-get"}); err == nil {
		t.Fatal("expected an error for HTTP 503, got nil")
	}
}

// Send is shared by every concurrent reconcile. Failing over to the secondary
// must not touch shared state: before, Send temporarily swapped BaseUrl, and an
// interleaving could "restore" the secondary as the permanent primary. Run with
// -race to also catch the unsynchronized access itself.
func TestSend_ConcurrentFailoverKeepsPrimary(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	primary := dead.URL
	dead.Close() // connections to primary are now refused

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"result":0,"text":"ok"}]`))
	}))
	defer secondary.Close()

	c := NewKeaClientWithOptions(OptionURL(primary), OptionSecondaryURL(secondary.URL))

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			resp, err := c.Send(context.Background(), keamodels.Request{Command: "version-get"})
			if err != nil || resp.Result != 0 {
				t.Errorf("expected failover success, got resp=%+v err=%v", resp, err)
			}
		})
	}
	wg.Wait()

	if c.BaseUrl != primary {
		t.Fatalf("primary URL changed after concurrent failovers: got %s, want %s", c.BaseUrl, primary)
	}
}
