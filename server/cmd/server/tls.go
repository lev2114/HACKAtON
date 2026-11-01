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

// ensureSelfSignedCert returns paths to a cert/key pair in dir, generating
// a new self-signed one on first run (and reusing it after that).
//
// There is no way to get a browser-trusted certificate for a LAN demo: real
// CAs validate ownership of a public domain over the internet, and we're
// serving an IP address with no internet access during the offline half of
// the demo. A self-signed cert is the practical option — every client's
// browser will show a one-time "connection is not private" warning that
// needs a manual "proceed anyway" click, but traffic (including the
// register/login password) is still encrypted in transit.
func ensureSelfSignedCert(dir string) (certFile, keyFile string, err error) {
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	if fileExists(certFile) && fileExists(keyFile) {
		return certFile, keyFile, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("cert dir: %w", err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return "", "", fmt.Errorf("serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Sharolet LAN demo"}, CommonName: "sharolet.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  localIPs(),
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return "", "", fmt.Errorf("create certificate: %w", err)
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return "", "", fmt.Errorf("open cert file: %w", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return "", "", fmt.Errorf("write cert: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", "", fmt.Errorf("marshal key: %w", err)
	}
	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("open key file: %w", err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return "", "", fmt.Errorf("write key: %w", err)
	}

	fmt.Printf(
		"Сгенерирован новый самоподписанный сертификат (%s) — в каждом браузере "+
			"при первом заходе нужно будет один раз подтвердить предупреждение "+
			"о безопасности (\"Дополнительно\" → \"Перейти на сайт\").\n",
		certFile,
	)
	return certFile, keyFile, nil
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
