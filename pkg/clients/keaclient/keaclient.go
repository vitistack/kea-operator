package keaclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"os"

	"github.com/spf13/viper"
	"github.com/vitistack/common/pkg/loggers/vlog"
	"github.com/vitistack/common/pkg/serialize"
	"github.com/vitistack/kea-operator/internal/consts"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

type keaClient struct {
	Context    context.Context
	BaseUrl    string
	Port       string
	HttpClient *http.Client

	// TLS options
	CACertPath         string
	ClientCertPath     string
	ClientKeyPath      string
	InsecureSkipVerify bool
	ServerName         string

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
	// Ensure HTTP client is built with TLS if configured
	c.buildHTTPClient()

	base, err := c.buildBaseURL()
	if err != nil {
		return keamodels.Response{}, err
	}
	keaCommand := serialize.JSON(cmd)
	body, _ := json.Marshal(keamodels.Request{Command: keaCommand})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/", bytes.NewReader(body))
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

	var out struct {
		Responses []keamodels.Response `json:"responses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return keamodels.Response{}, err
	}
	if len(out.Responses) == 0 {
		return keamodels.Response{}, errors.New("empty response")
	}
	return out.Responses[0], nil
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

	tlsNeeded := c.CACertPath != "" ||
		(c.ClientCertPath != "" && c.ClientKeyPath != "") ||
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
	if c.CACertPath != "" {
		caPEM, err := os.ReadFile(c.CACertPath)
		if err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(caPEM) {
				tlsCfg.RootCAs = pool
			}
		}
	}
	if c.ClientCertPath != "" && c.ClientKeyPath != "" {
		if cert, err := tls.LoadX509KeyPair(c.ClientCertPath, c.ClientKeyPath); err == nil {
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
	}
	transport := &http.Transport{TLSClientConfig: tlsCfg}
	c.HttpClient.Transport = transport
	c.HttpClient.Timeout = c.Timeout
}

// NewKeaClientFromEnv builds a Kea client using environment variables.
// Supported env vars:
//
//	KEA_BASE_URL (preferred, e.g. https://kea.example:8000) or KEA_HOST and optional KEA_PORT
//	KEA_TLS_CA_FILE, KEA_TLS_CERT_FILE, KEA_TLS_KEY_FILE
//	KEA_TLS_INSECURE (true/false)
//	KEA_TLS_SERVER_NAME
//	KEA_TIMEOUT_SECONDS (default 10)
func NewKeaClientFromEnv() *keaClient {
	// Ensure env is wired into Viper
	viper.AutomaticEnv()
	// Bind the exact env var names we expect
	_ = viper.BindEnv(consts.KEA_BASE_URL)
	_ = viper.BindEnv(consts.KEA_HOST)
	_ = viper.BindEnv(consts.KEA_PORT)
	_ = viper.BindEnv(consts.KEA_TLS_CA_FILE)
	_ = viper.BindEnv(consts.KEA_TLS_CERT_FILE)
	_ = viper.BindEnv(consts.KEA_TLS_KEY_FILE)
	_ = viper.BindEnv(consts.KEA_TLS_INSECURE)
	_ = viper.BindEnv(consts.KEA_TLS_SERVER_NAME)
	_ = viper.BindEnv(consts.KEA_TIMEOUT_SECONDS)

	base := viper.GetString(consts.KEA_BASE_URL)
	host := viper.GetString(consts.KEA_HOST)
	port := viper.GetString(consts.KEA_PORT)
	if base == "" && host != "" {
		base = host
	}
	kc := NewKeaClient(base, port)
	// TLS options
	kc.CACertPath = viper.GetString(consts.KEA_TLS_CA_FILE)
	kc.ClientCertPath = viper.GetString(consts.KEA_TLS_CERT_FILE)
	kc.ClientKeyPath = viper.GetString(consts.KEA_TLS_KEY_FILE)
	kc.InsecureSkipVerify = viper.GetBool(consts.KEA_TLS_INSECURE)
	kc.ServerName = viper.GetString(consts.KEA_TLS_SERVER_NAME)
	if secs := viper.GetInt(consts.KEA_TIMEOUT_SECONDS); secs > 0 {
		kc.Timeout = time.Duration(secs) * time.Second
	}
	kc.buildHTTPClient()
	return kc
}
