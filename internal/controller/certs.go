package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// GenerateCerts creates a private CA and a server certificate for the given
// hostnames and IPs. Agents trust only this CA, pinned by its fingerprint at install.
func GenerateCerts(cfg Config, hosts []string) error {
	if err := os.MkdirAll(filepath.Dir(cfg.CACert), 0o750); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: "SR-Net-Speed-Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, _ := x509.ParseCertificate(caDER)

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: "srnet-controller"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			srvTmpl.IPAddresses = append(srvTmpl.IPAddresses, ip)
		} else if h != "" {
			srvTmpl.DNSNames = append(srvTmpl.DNSNames, h)
		}
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return err
	}
	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return err
	}
	writes := []struct {
		path, typ string
		der       []byte
		mode      os.FileMode
	}{
		{cfg.CACert, "CERTIFICATE", caDER, 0o644},
		{filepath.Join(filepath.Dir(cfg.CACert), "ca-key.pem"), "EC PRIVATE KEY", caKeyDER, 0o600},
		{cfg.TLSCert, "CERTIFICATE", srvDER, 0o644},
		{cfg.TLSKey, "EC PRIVATE KEY", srvKeyDER, 0o600},
	}
	for _, w := range writes {
		if err := os.WriteFile(w.path, pem.EncodeToMemory(&pem.Block{Type: w.typ, Bytes: w.der}), w.mode); err != nil {
			return fmt.Errorf("write %s: %w", w.path, err)
		}
	}
	return nil
}

// CAFingerprint is the SHA-256 of the CA PEM file exactly as served at /ca.pem,
// so install.sh can check it with plain sha256sum.
func CAFingerprint(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func randSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
