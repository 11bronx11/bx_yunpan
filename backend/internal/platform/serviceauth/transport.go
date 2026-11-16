package serviceauth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"os"
)

type TLS struct{ CA, Cert, Key, ServerName string }

func (c TLS) Validate() error {
	if (c.Cert == "") != (c.Key == "") || c.Cert != "" && c.CA == "" {
		return errors.New("internal TLS requires CA, certificate and key together")
	}
	return nil
}
func tlsConfig(c TLS) (*tls.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.CA == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(c.CA)
	if err != nil {
		return nil, errors.New("cannot read internal TLS CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		return nil, errors.New("invalid internal TLS CA")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ClientCAs: roots, ServerName: c.ServerName}
	if c.Cert != "" {
		cert, err := tls.LoadX509KeyPair(c.Cert, c.Key)
		if err != nil {
			return nil, errors.New("invalid internal TLS certificate")
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
func ClientTransport(c TLS) (credentials.TransportCredentials, error) {
	cfg, err := tlsConfig(c)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return insecure.NewCredentials(), nil
	}
	return credentials.NewTLS(cfg), nil
}
func ServerTransport(c TLS) (credentials.TransportCredentials, error) {
	cfg, err := tlsConfig(c)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	if len(cfg.Certificates) == 0 {
		return nil, errors.New("internal TLS server certificate required")
	}
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return credentials.NewTLS(cfg), nil
}
