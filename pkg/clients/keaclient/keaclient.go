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

type keaClient struct {
	Context    context.Context
	BaseUrl    string
	Port       string
	HttpClient *http.Client

	lastConfigHash    string // simple hash to avoid rebuilding transport when unchanged
	disableKeepAlives bool

	// TLS options
	CACertPath         string
	ClientCertPath     string
	ClientKeyPath      string
	InsecureSkipVerify bool
	ServerName         string

	// Direct PEM data (takes precedence over file paths if provided)
	CACertPEM     []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte

	Timeout time.Duration
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
	kc.Timeout = 10 * time.Second
	// Default plain client; may be overridden by buildHTTPClient()
	kc.HttpClient = &http.Client{Timeout: kc.Timeout}
}

func (c *keaClient) Send(ctx context.Context, cmd keamodels.Request) (keamodels.Response, error) {
	// Ensure HTTP client is built (lazy) if config changed
	c.buildHTTPClient()
	base, err := c.buildBaseURL()
	if err != nil {
		return keamodels.Response{}, err
	}

	// Marshal the request exactly as provided (no double-encoding of command field)
	body, err := json.Marshal(cmd)
	if err != nil {
		return keamodels.Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/", bytes.NewReader(body))
	if err != nil {
		return keamodels.Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HttpClient.Do(req)
	if err != nil {
		return keamodels.Response{}, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			vlog.Error("failed to close response body: %v", cerr)
		}
	}()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return keamodels.Response{}, err
	}

	// 1. Try plain array response: [ { result, text, ... } ]
	var arr []keamodels.Response
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		return arr[0], nil
	}
	// 1b. Lax parse allowing non-object arguments (e.g., list-commands returns arguments as array)
	type laxResponse struct {
		Result    int             `json:"result"`
		Text      string          `json:"text"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var arrLax []laxResponse
	if err := json.Unmarshal(data, &arrLax); err == nil && len(arrLax) > 0 {
		return keamodels.Response{Result: arrLax[0].Result, Text: arrLax[0].Text}, nil
	}
	// 2. Try wrapped object: { "responses": [ ... ] }
	var wrapped struct {
		Responses []keamodels.Response `json:"responses"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Responses) > 0 {
		return wrapped.Responses[0], nil
	}
	// 2b. Lax wrapped parse
	var wrappedLax struct {
		Responses []laxResponse `json:"responses"`
	}
	if err := json.Unmarshal(data, &wrappedLax); err == nil && len(wrappedLax.Responses) > 0 {
		lr := wrappedLax.Responses[0]
		return keamodels.Response{Result: lr.Result, Text: lr.Text}, nil
	}
	// 3. Try single object (treat as valid even if text is empty and result == 0)
	var single keamodels.Response
	if err := json.Unmarshal(data, &single); err == nil {
		return single, nil
	}
	// 3b. Lax single object
	var singleLax laxResponse
	if err := json.Unmarshal(data, &singleLax); err == nil {
		return keamodels.Response{Result: singleLax.Result, Text: singleLax.Text}, nil
	}

	// Pretty-print JSON body when possible to aid debugging
	pretty := string(data)
	if len(data) > 0 {
		var buf bytes.Buffer
		if err := json.Indent(&buf, data, "", "  "); err == nil {
			pretty = buf.String()
		}
	}
	vlog.Warn("unexpected Kea response payload", "body", pretty)
	return keamodels.Response{}, errors.New("unrecognized Kea response format")
}

// buildBaseURL constructs a full base URL including scheme and port if needed.
func (c *keaClient) buildBaseURL() (string, error) {
	s := c.BaseUrl
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

// buildHTTPClient builds the HTTP client with TLS settings, if any are provided.
func (c *keaClient) buildHTTPClient() {
	// If already has a transport with TLS or no TLS requested, keep existing unless timeout changed
	// Always ensure timeout is applied
	if c.HttpClient == nil {
		c.HttpClient = &http.Client{}
	}

	// Compute a lightweight config fingerprint
	confParts := []string{
		c.BaseUrl, c.Port,
		c.CACertPath, c.ClientCertPath, c.ClientKeyPath,
		c.ServerName,
		boolToStr(c.InsecureSkipVerify),
		hashBytes(c.CACertPEM), hashBytes(c.ClientCertPEM), hashBytes(c.ClientKeyPEM),
	}
	newHash := strings.Join(confParts, "|")
	if c.lastConfigHash == newHash && c.HttpClient.Transport != nil {
		// Only update timeout
		c.HttpClient.Timeout = c.Timeout
		return
	}

	tlsNeeded := c.CACertPath != "" || len(c.CACertPEM) > 0 ||
		((c.ClientCertPath != "" && c.ClientKeyPath != "") || (len(c.ClientCertPEM) > 0 && len(c.ClientKeyPEM) > 0)) ||
		c.InsecureSkipVerify ||
		c.ServerName != ""
	if !tlsNeeded {
		c.HttpClient.Timeout = c.Timeout
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
				vlog.Warn("failed to append CA certs from %s", c.CACertPath)
			}
		} else {
			vlog.Warn("failed to read CA cert file %s: %v", c.CACertPath, err)
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
	transport := &http.Transport{TLSClientConfig: tlsCfg, DisableKeepAlives: c.disableKeepAlives}
	c.HttpClient.Transport = transport
	c.lastConfigHash = newHash
	c.HttpClient.Timeout = c.Timeout
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// hashBytes returns a short stable string for a byte slice:
// length plus first/last 4 bytes (hex) to avoid heavy hashing libraries.
func hashBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) < 8 {
		return fmt.Sprintf("%d:%x", len(b), b)
	}
	return fmt.Sprintf("%d:%x:%x", len(b), b[:4], b[len(b)-4:])
}

// loadClientCertWithFallback attempts to load the configured client cert/key first;
// if that fails, it tries conventional fallbacks in the same directory: client.crt/client.key and tls.crt/tls.key.
// Returns nil if no pair could be loaded.
func (c *keaClient) loadClientCertWithFallback() *tls.Certificate {
	pathsTried := [][2]string{{c.ClientCertPath, c.ClientKeyPath}}
	dir := filepath.Dir(c.ClientCertPath)
	// Add common alternative names
	pathsTried = append(pathsTried,
		[2]string{filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")},
		[2]string{filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")},
	)
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
		"no usable client certificate key pair found",
		"primaryCert", c.ClientCertPath,
		"primaryKey", c.ClientKeyPath,
	)
	return nil
}

// NewKeaClientFromEnv builds a Kea client using environment variables.
// Supported env vars:
//
//	KEA_BASE_URL (preferred, e.g. https://kea.example:8000) or KEA_HOST and optional KEA_PORT
//	KEA_TLS_CA_FILE, KEA_TLS_CERT_FILE, KEA_TLS_KEY_FILE
//	KEA_TLS_INSECURE (true/false)
//	KEA_TLS_SERVER_NAME
//	KEA_TIMEOUT_SECONDS (default 10)
//
// Deprecated: use NewKeaClientWithOptions(OptionFromEnv()) directly.
func NewKeaClientFromEnv() *keaClient {
	return NewKeaClientWithOptions(OptionFromEnv())
}
