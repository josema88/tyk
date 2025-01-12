package certs

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	cache "github.com/pmylund/go-cache"
)

// StorageHandler is a standard interface to a storage backend,
// used by AuthorisationManager to read and write key values to the backend
type StorageHandler interface {
	GetKey(string) (string, error)
	SetKey(string, string, int64) error
	GetKeys(string) []string
	DeleteKey(string) bool
	DeleteScanMatch(string) bool
}

type CertificateManager struct {
	storage StorageHandler
	logger  *logrus.Entry
	cache   *cache.Cache
	secret  string
}

func NewCertificateManager(storage StorageHandler, secret string, logger *logrus.Logger) *CertificateManager {
	if logger == nil {
		logger = logrus.New()
	}

	return &CertificateManager{
		storage: storage,
		logger:  logger.WithFields(logrus.Fields{"prefix": "cert_storage"}),
		cache:   cache.New(5*time.Minute, 10*time.Minute),
		secret:  secret,
	}
}

// Extracted from: https://golang.org/src/crypto/tls/tls.go
//
// Attempt to parse the given private key DER block. OpenSSL 0.9.8 generates
// PKCS#1 private keys by default, while OpenSSL 1.0.0 generates PKCS#8 keys.
// OpenSSL ecparam generates SEC1 EC private keys for ECDSA. We try all three.
func parsePrivateKey(der []byte) (crypto.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		switch key := key.(type) {
		case *rsa.PrivateKey, *ecdsa.PrivateKey:
			return key, nil
		default:
			return nil, errors.New("tls: found unknown private key type in PKCS#8 wrapping")
		}
	}

	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}

	return nil, errors.New("tls: failed to parse private key")
}

func isSHA256(value string) bool {
	// check if hex encoded
	if _, err := hex.DecodeString(value); err != nil {
		return false
	}

	return true
}

func HexSHA256(cert []byte) string {
	certSHA := sha256.Sum256(cert)
	return hex.EncodeToString(certSHA[:])
}

func ParsePEM(data []byte, secret string) ([]*pem.Block, error) {
	var pemBlocks []*pem.Block

	for {
		var block *pem.Block
		block, data = pem.Decode(data)

		if block == nil {
			break
		}

		if x509.IsEncryptedPEMBlock(block) {
			var err error
			block.Bytes, err = x509.DecryptPEMBlock(block, []byte(secret))
			block.Headers = nil
			block.Type = strings.Replace(block.Type, "ENCRYPTED ", "", 1)

			if err != nil {
				return nil, err
			}
		}

		pemBlocks = append(pemBlocks, block)
	}

	return pemBlocks, nil
}

func publicKey(priv interface{}) interface{} {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	default:
		return nil
	}
}

func ParsePEMCertificate(data []byte, secret string) (*tls.Certificate, error) {
	var cert tls.Certificate

	blocks, err := ParsePEM(data, secret)
	if err != nil {
		return nil, err
	}

	var certID string

	for _, block := range blocks {
		if block.Type == "CERTIFICATE" {
			certID = HexSHA256(block.Bytes)
			cert.Certificate = append(cert.Certificate, block.Bytes)
			continue
		}

		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			cert.PrivateKey, err = parsePrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			continue
		}

		if block.Type == "PUBLIC KEY" {
			// Create a dummny cert just for listing purpose
			cert.Certificate = append(cert.Certificate, block.Bytes)
			cert.Leaf = &x509.Certificate{Subject: pkix.Name{CommonName: "Public Key: " + HexSHA256(block.Bytes)}}
		}
	}

	if len(cert.Certificate) == 0 {
		return nil, errors.New("Can't find CERTIFICATE block")
	}

	if cert.Leaf == nil {
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])

		if err != nil {
			return nil, err
		}
	}

	// Cache certificate fingerprint
	cert.Leaf.Extensions = append([]pkix.Extension{{
		Value: []byte(certID),
	}}, cert.Leaf.Extensions...)

	return &cert, nil
}

type CertificateType int

const (
	CertificatePrivate CertificateType = iota
	CertificatePublic
	CertificateAny
)

func isPrivateKeyEmpty(cert *tls.Certificate) bool {
	switch priv := cert.PrivateKey.(type) {
	default:
		if priv == nil {
			return true
		}
	}

	return false
}

func isCertCanBeListed(cert *tls.Certificate, mode CertificateType) bool {
	switch mode {
	case CertificatePrivate:
		return !isPrivateKeyEmpty(cert)
	case CertificatePublic:
		return isPrivateKeyEmpty(cert)
	case CertificateAny:
		return true
	}

	return true
}

type CertificateMeta struct {
	ID            string    `json:"id"`
	Fingerprint   string    `json:"fingerprint"`
	HasPrivateKey bool      `json:"has_private"`
	Issuer        pkix.Name `json:"issuer,omitempty"`
	Subject       pkix.Name `json:"subject,omitempty"`
	NotBefore     time.Time `json:"not_before,omitempty"`
	NotAfter      time.Time `json:"not_after,omitempty"`
	DNSNames      []string  `json:"dns_names,omitempty"`
}

func ExtractCertificateMeta(cert *tls.Certificate, certID string) *CertificateMeta {
	return &CertificateMeta{
		ID:            certID,
		Fingerprint:   string(cert.Leaf.Extensions[0].Value),
		HasPrivateKey: !isPrivateKeyEmpty(cert),
		Issuer:        cert.Leaf.Issuer,
		Subject:       cert.Leaf.Subject,
		NotBefore:     cert.Leaf.NotBefore,
		NotAfter:      cert.Leaf.NotAfter,
		DNSNames:      cert.Leaf.DNSNames,
	}
}

func (c *CertificateManager) List(certIDs []string, mode CertificateType) (out []*tls.Certificate) {
	var cert *tls.Certificate
	var rawCert []byte
	var err error

	for _, id := range certIDs {
		if cert, found := c.cache.Get(id); found {
			if isCertCanBeListed(cert.(*tls.Certificate), mode) {
				out = append(out, cert.(*tls.Certificate))
			}
			continue
		}

		if isSHA256(id) {
			var val string
			val, err = c.storage.GetKey("raw-" + id)
			if err != nil {
				c.logger.Warn("Can't retrieve certificate from Redis:", id, err)
				out = append(out, nil)
				continue
			}
			rawCert = []byte(val)
		} else {
			rawCert, err = ioutil.ReadFile(id)
			if err != nil {
				c.logger.Error("Error while reading certificate from file:", id, err)
				out = append(out, nil)
				continue
			}
		}

		cert, err = ParsePEMCertificate(rawCert, c.secret)
		if err != nil {
			c.logger.Error("Error while parsing certificate: ", id, " ", err)
			c.logger.Debug("Failed certificate: ", string(rawCert))
			out = append(out, nil)
			continue
		}

		c.cache.Set(id, cert, cache.DefaultExpiration)

		if isCertCanBeListed(cert, mode) {
			out = append(out, cert)
		}
	}

	return out
}

// Returns list of fingerprints
func (c *CertificateManager) ListPublicKeys(keyIDs []string) (out []string) {
	var rawKey []byte
	var err error

	for _, id := range keyIDs {
		if fingerprint, found := c.cache.Get("pub-" + id); found {
			out = append(out, fingerprint.(string))
			continue
		}

		if isSHA256(id) {
			var val string
			val, err = c.storage.GetKey("raw-" + id)
			if err != nil {
				c.logger.Warn("Can't retrieve public key from Redis:", id, err)
				out = append(out, "")
				continue
			}
			rawKey = []byte(val)
		} else {
			rawKey, err = ioutil.ReadFile(id)
			if err != nil {
				c.logger.Error("Error while reading public key from file:", id, err)
				out = append(out, "")
				continue
			}
		}

		block, _ := pem.Decode(rawKey)
		if block == nil {
			c.logger.Error("Can't parse public key:", id)
			out = append(out, "")
			continue
		}

		fingerprint := HexSHA256(block.Bytes)
		c.cache.Set("pub-"+id, fingerprint, cache.DefaultExpiration)
		out = append(out, fingerprint)
	}

	return out
}

func (c *CertificateManager) ListAllIds(prefix string) (out []string) {
	keys := c.storage.GetKeys("raw-" + prefix + "*")

	for _, key := range keys {
		out = append(out, strings.TrimPrefix(key, "raw-"))
	}

	return out
}

func (c *CertificateManager) GetRaw(certID string) (string, error) {
	return c.storage.GetKey("raw-" + certID)
}

func (c *CertificateManager) Add(certData []byte, orgID string) (string, error) {
	var certBlocks [][]byte
	var keyPEM, keyRaw []byte
	var publicKeyPem []byte

	rest := certData

	for {
		var block *pem.Block

		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}

		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			if len(keyRaw) > 0 {
				err := errors.New("Found multiple private keys")
				c.logger.Error(err)
				return "", err
			}

			keyRaw = block.Bytes
			keyPEM = pem.EncodeToMemory(block)
		} else if block.Type == "CERTIFICATE" {
			certBlocks = append(certBlocks, pem.EncodeToMemory(block))
		} else if block.Type == "PUBLIC KEY" {
			publicKeyPem = pem.EncodeToMemory(block)
		} else {
			c.logger.Info("Ingnoring PEM block with type:", block.Type)
		}
	}

	certChainPEM := bytes.Join(certBlocks, []byte("\n"))

	if len(certChainPEM) == 0 {
		if len(publicKeyPem) == 0 {
			err := errors.New("Failed to decode certificate. It should be PEM encoded.")
			c.logger.Error(err)
			return "", err
		} else {
			certChainPEM = publicKeyPem
		}
	} else if len(publicKeyPem) > 0 {
		err := errors.New("Public keys can't be combined with certificates")
		c.logger.Error(err)
		return "", err
	}

	var certID string

	// Found private key, check if it match the certificate
	if len(keyPEM) > 0 {
		cert, err := tls.X509KeyPair(certChainPEM, keyPEM)
		if err != nil {
			c.logger.Error(err)
			return "", err
		}

		// Encrypt private key and append it to the chain
		encryptedKeyPEMBlock, err := x509.EncryptPEMBlock(rand.Reader, "ENCRYPTED PRIVATE KEY", keyRaw, []byte(c.secret), x509.PEMCipherAES256)
		if err != nil {
			c.logger.Error("Failed to encode private key", err)
			return "", err
		}

		certChainPEM = append(certChainPEM, []byte("\n")...)
		certChainPEM = append(certChainPEM, pem.EncodeToMemory(encryptedKeyPEMBlock)...)

		certID = orgID + HexSHA256(cert.Certificate[0])
	} else if len(publicKeyPem) > 0 {
		publicKey, _ := pem.Decode(publicKeyPem)
		certID = orgID + HexSHA256(publicKey.Bytes)
	} else {
		// Get first cert
		certRaw, _ := pem.Decode(certChainPEM)
		cert, err := x509.ParseCertificate(certRaw.Bytes)
		if err != nil {
			err := errors.New("Error while parsing certificate: " + err.Error())
			c.logger.Error(err)
			return "", err
		}

		certID = orgID + HexSHA256(cert.Raw)
	}

	if cert, err := c.storage.GetKey("raw-" + certID); err == nil && cert != "" {
		return "", errors.New("Certificate with " + certID + " id already exists")
	}

	if err := c.storage.SetKey("raw-"+certID, string(certChainPEM), 0); err != nil {
		c.logger.Error(err)
		return "", err
	}

	return certID, nil
}

func (c *CertificateManager) Delete(certID string) {
	c.storage.DeleteKey("raw-" + certID)
	c.cache.Delete(certID)
}

func (c *CertificateManager) CertPool(certIDs []string) *x509.CertPool {
	pool := x509.NewCertPool()

	for _, cert := range c.List(certIDs, CertificatePublic) {
		if cert != nil {
			pool.AddCert(cert.Leaf)
		}
	}

	return pool
}

func (c *CertificateManager) ValidateRequestCertificate(certIDs []string, r *http.Request) error {
	if r.TLS == nil {
		return errors.New("TLS not enabled")
	}

	if len(r.TLS.PeerCertificates) == 0 {
		return errors.New("Client TLS certificate is required")
	}

	leaf := r.TLS.PeerCertificates[0]

	certID := HexSHA256(leaf.Raw)
	for _, cert := range c.List(certIDs, CertificatePublic) {
		// Extensions[0] contains cache of certificate SHA256
		if cert == nil || string(cert.Leaf.Extensions[0].Value) == certID {
			return nil
		}
	}

	return errors.New("Certificate with SHA256 " + certID + " not allowed")
}

func (c *CertificateManager) FlushCache() {
	c.cache.Flush()
}

func (c *CertificateManager) flushStorage() {
	c.storage.DeleteScanMatch("*")
}
