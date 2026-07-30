package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// TLSConfig builds the server's TLS configuration, or returns nil when the
// endpoint URL is plain http.
//
// A browser page served over HTTPS cannot fetch an http:// URL - the request is
// blocked as mixed content before it is sent, and no response header can undo
// that. Serving this endpoint over HTTPS is the server-side half of the fix.
func (c *Config) TLSConfig() (*tls.Config, error) {
	if !c.Server.IsTLS() {
		return nil, nil
	}
	var cert tls.Certificate
	var err error
	switch {
	case c.Server.TLSCertFile != "" && c.Server.TLSKeyFile != "":
		cert, err = tls.LoadX509KeyPair(c.Server.TLSCertFile, c.Server.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("server.tls_cert_file/tls_key_file: %w", err)
		}
	case c.Server.TLSSelfSigned:
		cert, err = selfSignedCertificate(c.Server.TLSHosts())
		if err != nil {
			return nil, fmt.Errorf("could not generate a self-signed certificate: %w", err)
		}
	default:
		return nil, fmt.Errorf(
			"server.url is https but no certificate is configured; set server.tls_cert_file and "+
				"server.tls_key_file (or --tls-cert and --tls-key), or set server.tls_self_signed "+
				"(or --tls-self-signed) to generate one for %v", c.Server.TLSHosts())
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// IsTLS reports whether any configured endpoint URL is https. One certificate
// serves them all, so a single https endpoint is enough to need one.
func (s ServerConfig) IsTLS() bool {
	for _, raw := range s.URLs() {
		if strings.HasPrefix(raw, "https:") {
			return true
		}
	}
	return false
}

// TLSHosts is what a generated certificate is issued for: the host from the
// endpoint URL plus the loopback names, so the same certificate works however
// the client spells the address.
func (s ServerConfig) TLSHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	var named []string
	for _, raw := range s.URLs() {
		host := urlHost(raw)
		if host == "" {
			continue
		}
		found := false
		for _, existing := range append(hosts, named...) {
			if existing == host {
				found = true
			}
		}
		if !found {
			named = append(named, host)
		}
	}
	return append(named, hosts...)
}

// selfSignedCertificate mints an in-memory certificate for the given hosts.
//
// A browser will not trust it: the operator has to visit the URL once and
// accept the warning, or install a locally trusted certificate with a tool like
// mkcert. It exists so that HTTPS can be turned on without that ceremony first.
func selfSignedCertificate(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"codemcp"}, CommonName: hosts[0]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
