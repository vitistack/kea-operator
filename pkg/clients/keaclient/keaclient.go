package keaclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"os"

	"github.com/vitistack/common/pkg/loggers/vlog"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// errUnrecognizedResponse is returned when a Kea answer isn't a command response.
var errUnrecognizedResponse = errors.New("unrecognized Kea response format")

// keaClient talks to the Kea control API. One instance is shared by every
// concurrent reconcile, so after construction its fields are read-only: Send
// must never modify them.
type keaClient struct {
	Context      context.Context
	BaseUrl      string
	SecondaryUrl string // Optional secondary URL for HA failover
	Port         string
	HttpClient   *http.Client

	disableKeepAlives bool

	// TLS options
	CACertPath         string
	ClientCertPath     string
	ClientKeyPath      string
	InsecureSkipVerify bool
	ServerName         string

	// Basic auth (mutually exclusive with client cert usage).
	// If username provided and no cert/key provided, use basic auth.
	BasicAuthUsername string
	BasicAuthPassword string

	// Direct PEM data (takes precedence over file paths if provided)
	CACertPEM     []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte

	// Timeout bounds a whole request, including Kea's time to answer.
	Timeout time.Duration
	// ConnectTimeout bounds only TCP connect and TLS handshake, so an
	// unreachable server fails over quickly even with a long Timeout.
	ConnectTimeout time.Duration
}

func NewKeaClient(baseUrl, port string) *keaClient {
	kc := getDefaultKeaConnectionConfig()
	options := []KeaOption{
		OptionHost(baseUrl),
		OptionPort(port),
	}
	kc.applyOptions(options...)
	// Rebuild HTTP client with any provided TLS options
	kc.buildHTTPClient()
	return kc
}

// NewKeaClientWithOptions creates a client using functional options.
func NewKeaClientWithOptions(opts ...KeaOption) *keaClient {
	kc := getDefaultKeaConnectionConfig()
	kc.applyOptions(opts...)
	kc.buildHTTPClient()
	return kc
}

func getDefaultKeaConnectionConfig() *keaClient {
	kc := &keaClient{}
	kc.applyDefaults()
	return kc
}

func (kc *keaClient) applyOptions(options ...KeaOption) {
	for _, opt := range options {
		opt.apply(kc)
	}
}

func (kc *keaClient) applyDefaults() {
	kc.Context = context.Background()
	// 60s gives Kea — which serializes commands — room to answer under load
	// before we trip a timeout and force a failover. Override via
	// KEA_TIMEOUT_SECONDS.
	kc.Timeout = 60 * time.Second
	// Connecting is fast when the server is up; don't wait the full request
	// timeout before failing over. Override via KEA_CONNECT_TIMEOUT_SECONDS.
	kc.ConnectTimeout = 10 * time.Second
	// Default plain client; replaced by buildHTTPClient()
	kc.HttpClient = &http.Client{Timeout: kc.Timeout}
}

func (c *keaClient) Send(ctx context.Context, cmd keamodels.Request) (keamodels.Response, error) {
	// Marshal the request exactly as provided (no double-encoding of command field)
	body, err := json.Marshal(cmd)
	if err != nil {
		return keamodels.Response{}, err
	}

	// Try primary URL first, then secondary if available
	servers := []string{c.BaseUrl}
	if c.SecondaryUrl != "" {
		servers = append(servers, c.SecondaryUrl)
	}

	var lastErr error
	for i, server := range servers {
		base, err := c.buildURL(server)
		if err != nil {
			lastErr = fmt.Errorf("failed to build URL for %s: %w", server, err)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, "POST", base+"/", bytes.NewReader(body))
		if err != nil {
			lastErr = fmt.Errorf("failed to create request for %s: %w", base, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if c.BasicAuthUsername != "" && c.ClientCertPath == "" && len(c.ClientCertPEM) == 0 {
			req.SetBasicAuth(c.BasicAuthUsername, c.BasicAuthPassword)
		}

		// #nosec G704 -- URL is validated via buildURL() using url.Parse
		resp, err := c.HttpClient.Do(req)
		if err != nil {
			if i == 0 && len(servers) > 1 {
				vlog.Warnf("Primary KEA server failed, trying secondary. primary=%s error=%v", server, err)
			}
			lastErr = fmt.Errorf("request failed for %s: %w", base, err)
			continue
		}

		data, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); cerr != nil {
			vlog.Errorf("failed to close response body: %v", cerr)
		}
		if err != nil {
			lastErr = fmt.Errorf("failed to read response from %s: %w", base, err)
			continue
		}

		// Log successful failover if we're using secondary
		if i > 0 {
			vlog.Infof("Successfully failed over to secondary KEA server: url=%s", server)
		}

		// Kea returns command outcomes, failures included, in the body of an
		// HTTP 200. Any other status comes from auth, a proxy or a broken
		// server, so its body must not be read as a command result.
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return keamodels.Response{}, fmt.Errorf("kea server %s returned HTTP %d: %s", base, resp.StatusCode, snippet(data))
		}

		return c.parseResponse(data)
	}

	// All URLs failed
	return keamodels.Response{}, fmt.Errorf("all KEA servers failed: %w", lastErr)
}

// parseResponse decodes a Kea command response. Kea answers with an array
// (Control Agent), a single object (DHCP daemon HTTP socket) or an object
// wrapping a "responses" array; the first response is used. A body without a
// "result" field is rejected rather than read as success.
func (c *keaClient) parseResponse(data []byte) (keamodels.Response, error) {
	elem, err := firstResponseElement(data)
	if err != nil {
		logUnexpectedPayload(data)
		return keamodels.Response{}, err
	}

	var raw struct {
		Result    *int            `json:"result"`
		Text      string          `json:"text"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(elem, &raw); err != nil || raw.Result == nil {
		logUnexpectedPayload(data)
		return keamodels.Response{}, errUnrecognizedResponse
	}

	resp := keamodels.Response{Result: *raw.Result, Text: raw.Text}
	// Arguments that aren't an object (e.g. list-commands returns an array) are left nil.
	if len(raw.Arguments) > 0 {
		var args map[string]any
		if err := json.Unmarshal(raw.Arguments, &args); err == nil {
			resp.Arguments = args
		}
	}
	return resp, nil
}

// firstResponseElement returns the raw JSON of the first response in a Kea answer.
func firstResponseElement(data []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errUnrecognizedResponse
	}
	switch trimmed[0] {
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil || len(arr) == 0 {
			return nil, errUnrecognizedResponse
		}
		return arr[0], nil
	case '{':
		var wrapped struct {
			Responses *[]json.RawMessage `json:"responses"`
		}
		if err := json.Unmarshal(trimmed, &wrapped); err != nil {
			return nil, errUnrecognizedResponse
		}
		if wrapped.Responses == nil {
			return trimmed, nil
		}
		if len(*wrapped.Responses) == 0 {
			return nil, errUnrecognizedResponse
		}
		return (*wrapped.Responses)[0], nil
	}
	return nil, errUnrecognizedResponse
}

// logUnexpectedPayload pretty-prints a JSON body when possible to aid debugging.
func logUnexpectedPayload(data []byte) {
	pretty := string(data)
	if len(data) > 0 {
		var buf bytes.Buffer
		if err := json.Indent(&buf, data, "", "  "); err == nil {
			pretty = buf.String()
		}
	}
	vlog.Warn("unexpected Kea response payload", " body", pretty)
}

// snippet returns the start of a response body for error messages.
func snippet(data []byte) string {
	const maxLen = 200
	s := strings.TrimSpace(string(data))
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// buildURL constructs a full base URL for server (the primary or secondary
// URL), including scheme and port if needed.
func (c *keaClient) buildURL(server string) (string, error) {
	s := server
	if s == "" {
		return "", errors.New("base URL is empty")
	}
	s = strings.TrimRight(s, "/")
	if !strings.Contains(s, "://") {
		// Default to https if TLS certs are configured, else http
		if c.ClientCertPath != "" || c.CACertPath != "" {
			s = "https://" + s
		} else {
			s = "http://" + s
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		// No port present; add if provided
		if c.Port != "" {
			host = net.JoinHostPort(u.Hostname(), c.Port)
		}
	}
	u.Host = host
	return u.String(), nil
}

// buildHTTPClient builds the HTTP client, with TLS settings if any are
// provided. It runs once at construction; Send never rebuilds the client.
func (c *keaClient) buildHTTPClient() {
	if c.HttpClient == nil {
		c.HttpClient = &http.Client{}
	}
	c.HttpClient.Timeout = c.Timeout

	dialer := &net.Dialer{Timeout: c.ConnectTimeout, KeepAlive: 30 * time.Second}

	tlsNeeded := c.CACertPath != "" || len(c.CACertPEM) > 0 ||
		((c.ClientCertPath != "" && c.ClientKeyPath != "") || (len(c.ClientCertPEM) > 0 && len(c.ClientKeyPEM) > 0)) ||
		c.InsecureSkipVerify ||
		c.ServerName != ""
	if !tlsNeeded {
		// Same behavior as http.DefaultTransport, plus the connect timeout.
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = dt.Clone()
		}
		transport.DialContext = dialer.DialContext
		c.HttpClient.Transport = transport
		return
	}

	// #nosec G402 -- InsecureSkipVerify is intentionally allowed for test/dev usage.
	tlsCfg := &tls.Config{
		InsecureSkipVerify: c.InsecureSkipVerify,
	}
	if c.ServerName != "" {
		tlsCfg.ServerName = c.ServerName
	}
	if len(c.CACertPEM) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(c.CACertPEM) {
			tlsCfg.RootCAs = pool
		}
	} else if c.CACertPath != "" {
		if caPEM, err := os.ReadFile(c.CACertPath); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(caPEM) {
				tlsCfg.RootCAs = pool
			} else {
				vlog.Warnf("failed to append CA certs from %s", c.CACertPath)
			}
		} else {
			vlog.Warnf("failed to read CA cert file %s: %v", c.CACertPath, err)
		}
	}
	if len(c.ClientCertPEM) > 0 && len(c.ClientKeyPEM) > 0 {
		if cert, err := tls.X509KeyPair(c.ClientCertPEM, c.ClientKeyPEM); err == nil {
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
	} else if c.ClientCertPath != "" && c.ClientKeyPath != "" {
		if cert := c.loadClientCertWithFallback(); cert != nil {
			tlsCfg.Certificates = []tls.Certificate{*cert}
		}
	}
	c.HttpClient.Transport = &http.Transport{
		TLSClientConfig:     tlsCfg,
		DisableKeepAlives:   c.disableKeepAlives,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: c.ConnectTimeout,
	}
}

// loadClientCertWithFallback attempts to load the configured client cert/key first;
// if that fails, it tries conventional fallbacks in the same directory: client.crt/client.key and tls.crt/tls.key.
// Returns nil if no pair could be loaded.
func (c *keaClient) loadClientCertWithFallback() *tls.Certificate {
	dir := filepath.Dir(c.ClientCertPath)
	// Add common alternative names
	pathsTried := [][2]string{
		{c.ClientCertPath, c.ClientKeyPath},
		{filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")},
		{filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")},
	}
	for _, p := range pathsTried {
		certPath, keyPath := p[0], p[1]
		if certPath == "" || keyPath == "" {
			continue
		}
		if _, err := os.Stat(certPath); err != nil {
			continue
		}
		if _, err := os.Stat(keyPath); err != nil {
			continue
		}
		if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			if certPath != c.ClientCertPath || keyPath != c.ClientKeyPath {
				vlog.Info("loaded fallback client certificate", "cert", certPath, "key", keyPath)
			}
			return &cert
		}
	}
	vlog.Warn(
		"no usable client certificate key pair found ",
		"primaryCert: ", c.ClientCertPath,
		"primaryKey: ", c.ClientKeyPath,
	)
	return nil
}

// NewKeaClientFromEnv builds a Kea client using environment variables.
// Supported env vars:
//
//	KEA_URL (full URL with scheme, e.g. https://host:port) or KEA_BASE_URL + optional KEA_PORT
//	KEA_SECONDARY_URL (optional, for HA failover)
//	KEA_TLS_CA_FILE, KEA_TLS_CERT_FILE, KEA_TLS_KEY_FILE
//	KEA_TLS_INSECURE (true/false)
//	KEA_TLS_SERVER_NAME
//	KEA_TIMEOUT_SECONDS (default 60)
//	KEA_CONNECT_TIMEOUT_SECONDS (default 10)
//
// Deprecated: use NewKeaClientWithOptions(OptionFromEnv()) directly.
func NewKeaClientFromEnv() *keaClient {
	return NewKeaClientWithOptions(OptionFromEnv())
}
