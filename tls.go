package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"
)

func newHostTLS() (*tls.Config, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, "", err
	}
	certificate := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "ShadowChat ephemeral host"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	fingerprint := sha256.Sum256(der)
	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der}, PrivateKey: key,
		}},
	}
	return config, hex.EncodeToString(fingerprint[:]), nil
}

func pinnedTLS(fingerprint string) (*tls.Config, error) {
	expected, err := hex.DecodeString(fingerprint)
	if err != nil || len(expected) != sha256.Size {
		return nil, errors.New("fingerprint must be 64 hexadecimal SHA-256 characters")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// This self-signed certificate has no public CA or DNS identity. Mandatory
		// pin verification replaces normal PKI verification, never bypasses trust.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("host did not present a TLS certificate")
			}
			actual := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
				return fmt.Errorf("host certificate fingerprint mismatch (received %x)", actual)
			}
			return nil
		},
	}, nil
}
