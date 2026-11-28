package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/entitlement"
	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	"github.com/cuipengdba/dbsphere/internal/store"
)

func configureEntitlement(pg *store.Postgres) error {
	path := strings.TrimSpace(os.Getenv("DBSPHERE_ENTITLEMENT_PUBLIC_KEY_FILE"))
	encoded := strings.TrimSpace(os.Getenv("DBSPHERE_ENTITLEMENT_PUBLIC_KEY_PEM_BASE64"))
	if path == "" && encoded == "" {
		return nil
	}
	if path != "" && encoded != "" || len(encoded) > 8192 {
		return errors.New("configure exactly one entitlement public key source")
	}
	var wire []byte
	var err error
	if path != "" {
		wire, err = remoteobs.ReadFile(path, 4096)
	} else {
		wire, err = base64.StdEncoding.Strict().DecodeString(encoded)
	}
	if err != nil {
		return errors.New("cannot read entitlement public key")
	}
	key, err := entitlement.ParsePublicKey(wire)
	if err != nil {
		return errors.New("invalid entitlement public key")
	}
	pg.ConfigureEntitlement(key)
	return nil
}

func issueEntitlement(args []string) error {
	if len(args) != 2 {
		return errors.New("use license-issue PRIVATE_KEY CLAIMS_FILE; signed document is written to stdout")
	}
	wire, err := remoteobs.ReadFile(args[0], 4096)
	if err != nil {
		return errors.New("cannot read issuer key")
	}
	key, err := entitlement.ParsePrivateKey(wire)
	if err != nil {
		return err
	}
	claims, err := remoteobs.ReadFile(args[1], entitlement.MaxFileSize)
	if err != nil {
		return errors.New("cannot read license claims")
	}
	signed, err := entitlement.Issue(claims, key)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(signed, '\n'))
	return err
}

func runEntitlementClock(ctx context.Context, pg *store.Postgres, log *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := pg.CheckpointEntitlementClock(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Warn("entitlement clock checkpoint failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
