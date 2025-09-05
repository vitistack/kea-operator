package keaclient

import (
	"fmt"
	"strings"
	"time"
)

type KeaOption interface {
	apply(*keaClient)
}

type optionFunc func(*keaClient)

func (of optionFunc) apply(cfg *keaClient) { of(cfg) }

func OptionServerString(serverstring string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		var err error
		serverparts := strings.SplitN(serverstring, ":", 2)
		if len(serverparts) == 2 {
			cfg.BaseUrl = serverparts[0]
			cfg.Port = serverparts[1]
		} else {
			cfg.BaseUrl = serverparts[0]
			cfg.Port = "8000" // default KEA port
		}
		if cfg.BaseUrl == "" || cfg.Port == "" {
			fmt.Println("Error parsing server string: ", err)
		}
	})
}

func OptionHost(host string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.BaseUrl = host
	})
}

func OptionPort(port string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.Port = port
	})
}

// TLS and HTTP options
func OptionTLS(caFile, certFile, keyFile string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.CACertPath = caFile
		cfg.ClientCertPath = certFile
		cfg.ClientKeyPath = keyFile
	})
}

func OptionInsecureSkipVerify(insecure bool) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.InsecureSkipVerify = insecure
	})
}

func OptionServerName(serverName string) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.ServerName = serverName
	})
}

func OptionTimeout(d time.Duration) KeaOption {
	return optionFunc(func(cfg *keaClient) {
		cfg.Timeout = d
		if cfg.HttpClient != nil {
			cfg.HttpClient.Timeout = d
		}
	})
}
