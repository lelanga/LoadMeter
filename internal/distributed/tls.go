package distributed

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fingerprint is the lowercase hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	s := sha256.Sum256(der)
	return hex.EncodeToString(s[:])
}

// NormalizeFingerprint accepts "AB:CD:..", "sha256:abcd..", or plain hex.
func NormalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.TrimPrefix(s, "sha256/")
	return strings.ReplaceAll(s, ":", "")
}

// LoadOrCreateCert loads dir/cert.pem and dir/key.pem, generating a
// self-signed ECDSA P-256 certificate on first use. It returns the
// certificate and its SHA-256 fingerprint.
func LoadOrCreateCert(dir string) (tls.Certificate, string, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		if err := generateCert(dir, certPath, keyPath); err != nil {
			return tls.Certificate{}, "", err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("agent certificate: %w", err)
	}
	return cert, Fingerprint(cert.Certificate[0]), nil
}

func generateCert(dir, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "loadmeter-agent " + host, Organization: []string{"Load Meter agent (self-signed, pinned by fingerprint)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if host != "" {
		tpl.DNSNames = append(tpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// ServerTLS returns the agent's TLS configuration.
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

// ClientTLS pins the agent certificate by SHA-256 fingerprint. Chain and name
// verification are replaced by the pin (the certificate is self-signed), so a
// certificate with any other fingerprint is rejected.
func ClientTLS(fingerprint string) *tls.Config {
	want := NormalizeFingerprint(fingerprint)
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by the fingerprint pin below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("agent presented no certificate")
			}
			if got := Fingerprint(raw[0]); got != want || want == "" {
				return &FingerprintError{Want: want, Got: got}
			}
			return nil
		},
	}
}

// FingerprintError reports a certificate pin mismatch.
type FingerprintError struct{ Want, Got string }

func (e *FingerprintError) Error() string {
	return fmt.Sprintf("agent certificate fingerprint mismatch: expected sha256 %s, got %s (wrong agent, regenerated certificate, or interception)", e.Want, e.Got)
}

// LoadOrCreateToken reads the shared token from path, creating a new random
// 32-byte token (hex, mode 0600) when the file does not exist.
func LoadOrCreateToken(path string) (string, bool, error) {
	if path == "" {
		return "", false, errors.New("a token file is required (--token-file)")
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		b := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, b); err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", false, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", false, err
		}
		_, err = f.WriteString(hex.EncodeToString(b) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", false, err
		}
		tok, err := ReadToken(path)
		return tok, true, err
	}
	tok, err := ReadToken(path)
	return tok, false, err
}

// ReadToken reads and validates a token file.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < 32 {
		return "", fmt.Errorf("token in %s is too short (need at least 32 characters; delete the file to generate one)", path)
	}
	return tok, nil
}

// TokenFileTooOpen reports whether group/other can read the token file (Unix).
func TokenFileTooOpen(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().Perm()&0o077 != 0
}

func readFileLimited(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("files shipped to agents exceed %d MB in total", maxShippedFiles>>20)
	}
	return b, nil
}
