package remoteobs

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
)

func ReadFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, ErrInvalid
	}
	return data, nil
}

func LoadTLS(cert, key, ca string, server bool) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, errors.New("load collector TLS certificate and key")
	}
	pem, err := ReadFile(ca, 256<<10)
	pool := x509.NewCertPool()
	if err != nil || !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("load collector TLS trust bundle")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	if server {
		config.ClientAuth = tls.RequireAndVerifyClientCert
		config.ClientCAs = pool
	} else {
		config.RootCAs = pool
	}
	return config, nil
}
