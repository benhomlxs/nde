package tlsutil

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

func EnsureServerKeypair(certPath, keyPath string) error {
	if fileExists(certPath) && fileExists(keyPath) {
		return nil
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "marznode.local",
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return nil
}

func BuildServerTLSConfig(certPath, keyPath, clientCertPath string) (*tls.Config, error) {
	if clientCertPath == "" {
		return nil, errors.New("missing client certificate")
	}
	serverCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	clientPEM, err := os.ReadFile(clientCertPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(clientPEM) {
		return nil, errors.New("invalid client certificate")
	}
	trusted := parsePEMCerts(clientPEM)
	if len(trusted) == 0 {
		return nil, errors.New("no cert found in client certificate bundle")
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		// Python version accepted client certs loaded from SSL_CLIENT_CERT_FILE directly.
		// To keep compatibility with deployments where this file is a leaf cert (not a CA),
		// we verify manually and allow direct leaf match as fallback.
		ClientAuth: tls.RequireAnyClientCert,
		NextProtos: []string{"h2"},
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no client certificate")
			}

			certs := make([]*x509.Certificate, 0, len(rawCerts))
			for _, asn1Data := range rawCerts {
				c, err := x509.ParseCertificate(asn1Data)
				if err != nil {
					return fmt.Errorf("parse client cert: %w", err)
				}
				certs = append(certs, c)
			}

			leaf := certs[0]
			intermediates := x509.NewCertPool()
			for _, ic := range certs[1:] {
				intermediates.AddCert(ic)
			}

			if _, err := leaf.Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			}); err == nil {
				return nil
			}

			// Fallback: exact cert pinning (compat with old setups passing leaf cert).
			for _, tc := range trusted {
				if bytes.Equal(tc.Raw, leaf.Raw) {
					return nil
				}
			}
			return errors.New("client certificate is not trusted")
		},
	}, nil
}

func parsePEMCerts(pemData []byte) []*x509.Certificate {
	var certs []*x509.Certificate
	rest := pemData
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		certs = append(certs, c)
	}
	return certs
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
