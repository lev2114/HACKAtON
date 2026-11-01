package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ensureTLSCert returns paths to a server cert/key pair in dir, signed by a
// local root CA (also generated into dir, reused across restarts).
//
// There is no way to get a PUBLIC-CA-trusted certificate for a LAN demo:
// real CAs validate ownership of a public domain over the internet, and
// we're serving an IP address with no internet access during the offline
// half of the demo. The next best thing — and what makes the browser show
// an actual green "secure" padlock with no warning — is our OWN local CA:
// we generate a root certificate once, every client imports just that one
// *public* file into their OS's trusted root store (one-time, a few clicks,
// see docs/https-local-ca.md), and every server certificate we issue after
// that is automatically trusted by any client that did the import. The CA's
// PRIVATE key never leaves the server machine.
func ensureTLSCert(dir string) (certFile, keyFile string, err error) {
	caCertFile := filepath.Join(dir, "ca-cert.pem")
	caKeyFile := filepath.Join(dir, "ca-key.pem")
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	// Treat the whole chain as one unit: if any piece is missing, regenerate
	// all of it together so the leaf is always signed by the CA that's
	// actually on disk (an orphaned old leaf signed by a since-deleted CA
	// would just be a second self-signed-like cert nobody trusts).
	if fileExists(caCertFile) && fileExists(caKeyFile) && fileExists(certFile) && fileExists(keyFile) {
		return certFile, keyFile, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("cert dir: %w", err)
	}

	caCert, caKey, err := generateRootCA(caCertFile, caKeyFile)
	if err != nil {
		return "", "", fmt.Errorf("root CA: %w", err)
	}

	if err := generateLeafCert(certFile, keyFile, caCert, caKey); err != nil {
		return "", "", fmt.Errorf("server certificate: %w", err)
	}

	fmt.Printf(
		"\nСгенерирован новый локальный центр сертификации: %s\n"+
			"Чтобы в браузере не было предупреждения \"соединение не защищено\", "+
			"этот файл (именно этот, НЕ ca-key.pem) нужно один раз добавить в "+
			"доверенные корневые сертификаты на КАЖДОМ ноутбуке — см. "+
			"docs/https-local-ca.md. Без этого шага игра всё равно работает, "+
			"просто с предупреждением браузера.\n\n",
		caCertFile,
	)
	return certFile, keyFile, nil
}

func generateRootCA(certFile, keyFile string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Sharolet LAN demo"}, CommonName: "Sharolet Local CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal CA key: %w", err)
	}
	// The CA private key signs anything a client would then trust — keep it
	// server-side only and more locked down than the leaf key.
	if err := writePEM(keyFile, "EC PRIVATE KEY", keyBytes, 0o600); err != nil {
		return nil, nil, err
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return cert, priv, nil
}

func generateLeafCert(certFile, keyFile string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Sharolet LAN demo"}, CommonName: "sharolet.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  localIPs(),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &priv.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	return writePEM(keyFile, "EC PRIVATE KEY", keyBytes, 0o600)
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return serial, nil
}

func writePEM(path, blockType string, bytes []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: bytes}); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// localIPs lists every non-loopback IPv4 address this machine currently has,
// plus 127.0.0.1 — covers "open from localhost" and "open from another
// laptop on the same LAN" (whatever the server's current LAN IP turns out
// to be) without needing to know that IP ahead of time.
func localIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1")}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			ips = append(ips, ip4)
		}
	}
	return ips
}
