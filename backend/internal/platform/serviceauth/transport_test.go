package serviceauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// A08: real TCP/gRPC handshakes, including mutual client authentication.
// Deployment certificate distribution is separately exercised by the E2 runner.
func TestMutualTLSHandshakeMatrix(t *testing.T) {
	dir := t.TempDir()
	serial := int64(0)
	create := func(name string, parent *x509.Certificate, signer *ecdsa.PrivateKey, ca, expired bool) (*x509.Certificate, *ecdsa.PrivateKey, TLS) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		serial++
		tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true, IsCA: ca}
		if ca {
			tpl.KeyUsage |= x509.KeyUsageCertSign
		}
		if expired {
			tpl.NotAfter = time.Now().Add(-time.Minute)
		}
		if parent == nil {
			parent, signer = tpl, key
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(der)
		kb, _ := x509.MarshalECPrivateKey(key)
		c := TLS{Cert: filepath.Join(dir, name+".pem"), Key: filepath.Join(dir, name+".key"), ServerName: "core.test"}
		if err := os.WriteFile(c.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.Key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); err != nil {
			t.Fatal(err)
		}
		return cert, key, c
	}
	ca, caKey, root := create("ca", nil, nil, true, false)
	_, _, server := create("core.test", ca, caKey, false, false)
	_, _, client := create("client.test", ca, caKey, false, false)
	_, _, expired := create("expired.test", ca, caKey, false, true)
	_, _, wrongCA := create("wrong-ca", nil, nil, true, false)
	server.CA, client.CA, expired.CA = root.Cert, root.Cert, root.Cert
	transport, err := ServerTransport(server)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(transport))
	healthpb.RegisterHealthServer(s, health.NewServer())
	go s.Serve(listener)
	t.Cleanup(s.Stop)
	for _, tc := range []struct {
		name           string
		config         TLS
		plain, allowed bool
	}{
		{"valid", client, false, true},
		{"wrong-ca", TLS{CA: wrongCA.Cert, Cert: client.Cert, Key: client.Key, ServerName: "core.test"}, false, false},
		{"wrong-san", TLS{CA: root.Cert, Cert: client.Cert, Key: client.Key, ServerName: "wrong.test"}, false, false},
		{"missing-client", TLS{CA: root.Cert, ServerName: "core.test"}, false, false},
		{"expired-client", expired, false, false},
		{"plaintext", TLS{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := ClientTransport(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			if tc.plain {
				creds = insecure.NewCredentials()
			}
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(creds))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			if (err == nil) != tc.allowed {
				t.Fatalf("handshake allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
	mismatch := client
	mismatch.Key = server.Key
	if _, err := ClientTransport(mismatch); err == nil {
		t.Fatal("certificate/key mismatch accepted")
	}
	if _, err := ServerTransport(TLS{CA: root.Cert}); err == nil {
		t.Fatal("server without certificate accepted")
	}
	if _, err := ClientTransport(TLS{Cert: client.Cert}); err == nil {
		t.Fatal("partial TLS accepted")
	}
	if container := os.Getenv("TEST_AI_MTLS_CORE_CONTAINER"); container != "" {
		t.Run("deployed-core", func(t *testing.T) {
			testDeployedCoreTLS(t, container, dir, server, client, expired, wrongCA, root)
		})
	}
}
