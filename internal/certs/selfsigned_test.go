package certs

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

// TestSelfSignedIsAServerCertificateForItsHosts: the pair loads as a TLS
// key pair, names every host it was asked for, and is signed by itself.
func TestSelfSignedIsAServerCertificateForItsHosts(t *testing.T) {
	pair, err := SelfSigned("backup.example.net", []string{"backup.example.net", "203.0.113.7", ""}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM); err != nil {
		t.Fatalf("the pair does not load as a TLS key pair: %v", err)
	}
	block, _ := pem.Decode(pair.CertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("backup.example.net"); err != nil {
		t.Errorf("the certificate does not name its host: %v", err)
	}
	if err := cert.VerifyHostname("203.0.113.7"); err != nil {
		t.Errorf("the certificate does not name its address: %v", err)
	}
	if len(cert.DNSNames) != 1 {
		t.Errorf("an empty host became a name: %v", cert.DNSNames)
	}
	// CheckSignatureFrom wants a CA for a parent, and this is a leaf that
	// signed itself; the signature is checked against its own key.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("the certificate is not signed by itself: %v", err)
	}
	if cert.IsCA {
		t.Error("a server certificate claims to be a CA")
	}
	if !cert.NotAfter.After(time.Now().Add(23 * time.Hour)) {
		t.Errorf("the certificate expires too soon: %v", cert.NotAfter)
	}
}
