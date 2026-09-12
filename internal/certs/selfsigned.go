package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"time"
)

// SelfSigned makes a server certificate that vouches for itself.
//
// It is for the browser interface of a server that has no panel and no
// authority to ask: the operator compares the fingerprint the service
// logs with the one the browser shows, once, and the browser remembers
// it. Nothing else in Gniza trusts a certificate on the strength of its
// own signature; agents are still pinned by fingerprint against a CA.
func SelfSigned(commonName string, hosts []string, validFor time.Duration) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, fmt.Errorf("certs: generate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Pair{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"gniza"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if host != "" {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return Pair{}, fmt.Errorf("certs: self-sign certificate: %w", err)
	}
	return encodePair(der, key)
}
