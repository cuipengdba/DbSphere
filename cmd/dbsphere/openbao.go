package main

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/cuipengdba/dbsphere/internal/security"
)

func loadOpenBao() (*security.OpenBao, error) {
	address := os.Getenv("DBSPHERE_OPENBAO_URL")
	bindings := os.Getenv("DBSPHERE_OPENBAO_BINDINGS")
	ca64 := os.Getenv("DBSPHERE_OPENBAO_CA_PEM_BASE64")
	tokenFile := os.Getenv("DBSPHERE_OPENBAO_TOKEN_FILE")
	if address == "" && bindings == "" && ca64 == "" && tokenFile == "" {
		return nil, nil
	}
	if !filepath.IsAbs(tokenFile) || len(ca64) > 256*1024 {
		return nil, errors.New("OpenBao requires an absolute token file path and a bounded CA bundle")
	}
	ca, err := base64.StdEncoding.DecodeString(ca64)
	if err != nil {
		return nil, errors.New("OpenBao CA bundle must be base64 PEM")
	}
	return security.NewOpenBao(address, bindings, ca, func() (string, error) {
		file, err := os.Open(tokenFile)
		if err != nil {
			return "", err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0007 != 0 {
			return "", errors.New("OpenBao token file must be private and regular")
		}
		raw, err := io.ReadAll(io.LimitReader(file, 4097))
		if err != nil || len(raw) > 4096 {
			return "", errors.New("OpenBao token file is unavailable or oversized")
		}
		return string(raw), nil
	})
}
