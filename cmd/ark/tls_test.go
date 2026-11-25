package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"testing"
)

func TestEnsureSelfSignedTLSCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := ensureSelfSignedTLSCert(dir)
	if err != nil {
		t.Fatalf("ensureSelfSignedTLSCert failed: %v", err)
	}

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair failed: %v", err)
	}
	if len(pair.Certificate) == 0 {
		t.Fatalf("expected non-empty certificate chain")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate failed: %v", err)
	}

	hasLocalhost := false
	for _, dns := range leaf.DNSNames {
		if dns == "localhost" {
			hasLocalhost = true
			break
		}
	}
	if !hasLocalhost {
		t.Errorf("expected localhost in cert DNSNames, got %v", leaf.DNSNames)
	}
	hasLoopback := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			hasLoopback = true
			break
		}
	}
	if !hasLoopback {
		t.Errorf("expected 127.0.0.1 in cert IPAddresses, got %v", leaf.IPAddresses)
	}

	// Second call must reuse the existing cert/key without regenerating.
	info1, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	certPath2, keyPath2, err := ensureSelfSignedTLSCert(dir)
	if err != nil {
		t.Fatalf("second ensureSelfSignedTLSCert failed: %v", err)
	}
	if certPath2 != certPath || keyPath2 != keyPath {
		t.Errorf("expected identical paths on reuse, got (%q,%q) vs (%q,%q)", certPath2, keyPath2, certPath, keyPath)
	}
	info2, err := os.Stat(certPath2)
	if err != nil {
		t.Fatalf("stat cert 2: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("expected existing cert to be reused without rewriting")
	}
}
