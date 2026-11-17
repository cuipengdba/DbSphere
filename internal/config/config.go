package config

import (
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strconv"
)

type Config struct {
	ListenAddr  string
	DatabaseURL string
	MasterKey   []byte
	AdminPass   string
}

func Load() (Config, error) {
	url := os.Getenv("DBSPHERE_DATABASE_URL")
	if url == "" {
		return Config{}, errors.New("DBSPHERE_DATABASE_URL must point to an independent PostgreSQL 18 database")
	}
	password := os.Getenv("DBSPHERE_ADMIN_PASSWORD")
	if len(password) < 12 {
		return Config{}, errors.New("DBSPHERE_ADMIN_PASSWORD must contain at least 12 characters")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("DBSPHERE_MASTER_KEY"))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("DBSPHERE_MASTER_KEY must be a base64-encoded 32-byte key")
	}
	host := os.Getenv("DBSPHERE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 8080
	if value := os.Getenv("DBSPHERE_PORT"); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, errors.New("DBSPHERE_PORT must be between 1 and 65535")
		}
	}
	return Config{ListenAddr: net.JoinHostPort(host, strconv.Itoa(port)),
		DatabaseURL: url, MasterKey: key, AdminPass: password}, nil
}
