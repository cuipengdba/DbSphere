package store

import (
	"context"
	"errors"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
)

func (s *Postgres) collectionCredential(ctx context.Context, id string, target collector.Target, encrypted string, key []byte) (string, int, error) {
	if !s.openBao.Bound(id) {
		password, err := security.Decrypt(key, encrypted)
		return password, 0, err
	}
	if !assets.IsPostgreSQLRuntime(target.Runtime) || target.SSLMode != "require" {
		return "", 0, errors.New("OpenBao bindings require PostgreSQL with verified TLS")
	}
	secret, err := s.openBao.Read(ctx, id, target.Host, target.Port, target.Database, target.Username)
	return secret.Password, secret.Version, err
}
