package keaclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// keaServer is a test Kea endpoint that counts the requests it receives.
type keaServer struct {
	*httptest.Server
	hits atomic.Int32
}

// newKeaServer starts a server that lets answer respond to each request; hit
// is the 1-based number of the request. The body is read first, as Kea does:
// until it is, the server can't notice a client giving up on the request.
func newKeaServer(t *testing.T, answer func(hit int32, w http.ResponseWriter, r *http.Request)) *keaServer {
	t.Helper()
	s := &keaServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		answer(s.hits.Add(1), w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func answerOK(_ int32, w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`[{"result":0,"text":"ok"}]`))
}

func answerStatus(code int) func(int32, http.ResponseWriter, *http.Request) {
	return func(_ int32, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}
}

// hang never answers; the client's request timeout has to give up on it.
func hang(_ http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func versionGet() keamodels.Request { return keamodels.Request{Command: "version-get"} }

// fastRetries keeps retry backoff out of the test run time.
func fastRetries(n int) []KeaOption {
	return []KeaOption{OptionPrimaryRetries(n), OptionRetryBackoff(time.Millisecond, time.Millisecond)}
}

func newTestClient(primary string, backups []string, opts ...KeaOption) *keaClient {
	all := make([]KeaOption, 0, 2+len(opts))
	all = append(all, OptionURL(primary), OptionSecondaryURLs(backups...))
	return NewKeaClientWithOptions(append(all, opts...)...)
}

// A primary that times out a couple of times but then answers must keep the
// request: before, the first timeout sent it to the secondary.
func TestSend_RetriesPrimaryOnTimeoutBeforeFailover(t *testing.T) {
	primary := newKeaServer(t, func(hit int32, w http.ResponseWriter, r *http.Request) {
		if hit <= 2 {
			hang(w, r)
			return
		}
		answerOK(hit, w, r)
	})
	backup := newKeaServer(t, answerOK)

	c := newTestClient(primary.URL, []string{backup.URL},
		append(fastRetries(3), OptionTimeout(200*time.Millisecond))...)

	resp, err := c.Send(context.Background(), versionGet())
	if err != nil || resp.Result != 0 {
		t.Fatalf("expected success from the primary, got resp=%+v err=%v", resp, err)
	}
	if got := primary.hits.Load(); got != 3 {
		t.Fatalf("primary got %d requests, want 3 (two timeouts, then the answer)", got)
	}
	if got := backup.hits.Load(); got != 0 {
		t.Fatalf("backup got %d requests, want 0 while the primary recovers within its retries", got)
	}
}

// Once the primary has used up its retries, backups are asked in list order,
// once each, and the first one that answers wins.
func TestSend_FailsOverInOrderAfterPrimaryRetriesExhausted(t *testing.T) {
	primary := newKeaServer(t, answerStatus(http.StatusServiceUnavailable))
	backup1 := newKeaServer(t, answerStatus(http.StatusBadGateway))
	backup2 := newKeaServer(t, answerOK)
	backup3 := newKeaServer(t, answerOK)

	c := newTestClient(primary.URL, []string{backup1.URL, backup2.URL, backup3.URL}, fastRetries(2)...)

	resp, err := c.Send(context.Background(), versionGet())
	if err != nil || resp.Result != 0 {
		t.Fatalf("expected success from a backup, got resp=%+v err=%v", resp, err)
	}
	for _, tc := range []struct {
		name string
		srv  *keaServer
		want int32
	}{
		{"primary", primary, 3},
		{"backup1", backup1, 1},
		{"backup2", backup2, 1},
		{"backup3", backup3, 0},
	} {
		if got := tc.srv.hits.Load(); got != tc.want {
			t.Errorf("%s got %d requests, want %d", tc.name, got, tc.want)
		}
	}
}

// An HTTP error that isn't a gateway/availability failure (auth, a missing
// path) is an answer about the request, not about the server being down:
// retrying or failing over would not change it.
func TestSend_NonRetryableStatusNeitherRetriedNorFailedOver(t *testing.T) {
	primary := newKeaServer(t, answerStatus(http.StatusUnauthorized))
	backup := newKeaServer(t, answerOK)

	c := newTestClient(primary.URL, []string{backup.URL}, fastRetries(3)...)

	if _, err := c.Send(context.Background(), versionGet()); err == nil {
		t.Fatal("expected an error for HTTP 401, got nil")
	}
	if got := primary.hits.Load(); got != 1 {
		t.Errorf("primary got %d requests, want 1", got)
	}
	if got := backup.hits.Load(); got != 0 {
		t.Errorf("backup got %d requests, want 0", got)
	}
}

// After a failover the primary is left alone for the cooldown, so an outage
// doesn't cost every request the full retry cycle.
func TestSend_PrimaryCooldownSendsStraightToBackups(t *testing.T) {
	primary := newKeaServer(t, answerStatus(http.StatusServiceUnavailable))
	backup := newKeaServer(t, answerOK)

	c := newTestClient(primary.URL, []string{backup.URL},
		append(fastRetries(1), OptionPrimaryCooldown(time.Hour))...)

	for i := range 2 {
		if _, err := c.Send(context.Background(), versionGet()); err != nil {
			t.Fatalf("send %d: expected success from the backup, got %v", i+1, err)
		}
	}
	if got := primary.hits.Load(); got != 2 {
		t.Errorf("primary got %d requests, want 2 (first send only: 1 try + 1 retry)", got)
	}
	if got := backup.hits.Load(); got != 2 {
		t.Errorf("backup got %d requests, want 2", got)
	}
}

// When the cooldown runs out, the primary is asked first again.
func TestSend_PrimaryUsedAgainAfterCooldown(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	primary := newKeaServer(t, func(hit int32, w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		answerOK(hit, w, r)
	})
	backup := newKeaServer(t, answerOK)

	c := newTestClient(primary.URL, []string{backup.URL},
		append(fastRetries(0), OptionPrimaryCooldown(50*time.Millisecond))...)

	if _, err := c.Send(context.Background(), versionGet()); err != nil {
		t.Fatalf("send 1: expected success from the backup, got %v", err)
	}
	failing.Store(false)
	time.Sleep(100 * time.Millisecond)

	for i := range 2 {
		if _, err := c.Send(context.Background(), versionGet()); err != nil {
			t.Fatalf("send %d: expected success from the primary, got %v", i+2, err)
		}
	}
	if got := backup.hits.Load(); got != 1 {
		t.Errorf("backup got %d requests, want 1 (only the send during the outage)", got)
	}
	if got := primary.hits.Load(); got != 3 {
		t.Errorf("primary got %d requests, want 3", got)
	}
}

// If every backup fails while the primary is cooling down, the primary is
// still asked once before the request is given up on.
func TestSend_CooldownFallsBackToPrimaryWhenBackupsFail(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	primary := newKeaServer(t, func(hit int32, w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		answerOK(hit, w, r)
	})
	backup := newKeaServer(t, answerStatus(http.StatusServiceUnavailable))

	c := newTestClient(primary.URL, []string{backup.URL},
		append(fastRetries(1), OptionPrimaryCooldown(time.Hour))...)

	if _, err := c.Send(context.Background(), versionGet()); err == nil {
		t.Fatal("send 1: expected an error with every server failing")
	}
	failing.Store(false)

	resp, err := c.Send(context.Background(), versionGet())
	if err != nil || resp.Result != 0 {
		t.Fatalf("send 2: expected the primary to answer as a last resort, got resp=%+v err=%v", resp, err)
	}
	if got := backup.hits.Load(); got != 2 {
		t.Errorf("backup got %d requests, want 2 (asked first on both sends)", got)
	}
	if got := primary.hits.Load(); got != 3 {
		t.Errorf("primary got %d requests, want 3 (2 on send 1, the last resort on send 2)", got)
	}
}

// With no backups there is nothing to fail over to, so there is no cooldown:
// every request gets the primary's full retries.
func TestSend_NoBackupsMeansNoCooldown(t *testing.T) {
	primary := newKeaServer(t, func(hit int32, w http.ResponseWriter, r *http.Request) {
		if hit <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		answerOK(hit, w, r)
	})

	c := newTestClient(primary.URL, nil, append(fastRetries(1), OptionPrimaryCooldown(time.Hour))...)

	if _, err := c.Send(context.Background(), versionGet()); err == nil {
		t.Fatal("send 1: expected an error after the primary's retries")
	}
	resp, err := c.Send(context.Background(), versionGet())
	if err != nil || resp.Result != 0 {
		t.Fatalf("send 2: expected the primary to answer on its retry, got resp=%+v err=%v", resp, err)
	}
	if got := primary.hits.Load(); got != 4 {
		t.Errorf("primary got %d requests, want 4", got)
	}
}

// A cancelled request stops waiting between retries and does not move on to
// the backups.
func TestSend_ContextCancelStopsRetryBackoff(t *testing.T) {
	primary := newKeaServer(t, answerStatus(http.StatusServiceUnavailable))
	backup := newKeaServer(t, answerOK)

	c := newTestClient(primary.URL, []string{backup.URL},
		OptionPrimaryRetries(5), OptionRetryBackoff(time.Hour, time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Send(ctx, versionGet())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Send took %s after the context expired; the backoff ignored cancellation", elapsed)
	}
	if got := backup.hits.Load(); got != 0 {
		t.Errorf("backup got %d requests, want 0 after cancellation", got)
	}
}

// Retry delays double from the initial backoff, never exceed the maximum
// (jitter included) and don't overflow on high attempt numbers.
func TestRetryDelay_DoublesAndCaps(t *testing.T) {
	c := &keaClient{RetryBackoff: 100 * time.Millisecond, RetryMaxBackoff: 300 * time.Millisecond}
	for _, tc := range []struct {
		attempt int
		lo, hi  time.Duration
	}{
		{0, 100 * time.Millisecond, 125 * time.Millisecond},
		{1, 200 * time.Millisecond, 250 * time.Millisecond},
		{2, 300 * time.Millisecond, 300 * time.Millisecond},
		{70, 300 * time.Millisecond, 300 * time.Millisecond},
	} {
		for range 20 {
			if got := c.retryDelay(tc.attempt); got < tc.lo || got > tc.hi {
				t.Fatalf("retryDelay(%d) = %s, want within [%s, %s]", tc.attempt, got, tc.lo, tc.hi)
			}
		}
	}
}
