package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type prometheusDiscoveryStore interface {
	NodeExporterDiscovery(context.Context) (json.RawMessage, error)
}

func runPrometheusDiscovery(ctx context.Context, db prometheusDiscoveryStore, target string, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		value, err := db.NodeExporterDiscovery(readCtx)
		cancel()
		if err == nil {
			err = publishPrometheusTargets(target, value)
		}
		if err != nil && ctx.Err() == nil {
			log.Warn("Prometheus target discovery could not be refreshed", "error", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func publishPrometheusTargets(target string, value []byte) error {
	existing, err := os.ReadFile(target)
	if err == nil && bytes.Equal(existing, value) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".dbsphere-targets-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), target)
}
