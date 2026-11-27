package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/cuipengdba/dbsphere/internal/config"
	"github.com/cuipengdba/dbsphere/internal/store"
)

func manageEmergencyAdmin(args []string) error {
	var minutes int
	switch {
	case len(args) == 1 && args[0] == "review":
	case len(args) == 2 && args[0] == "enable":
		var err error
		minutes, err = strconv.Atoi(args[1])
		if err != nil || minutes < 1 || minutes > 60 {
			return errors.New("emergency administrator duration must be 1 to 60 minutes")
		}
	default:
		return errors.New("usage: dbsphere emergency-admin enable <minutes> | review")
	}
	if os.Getenv("DBSPHERE_EMERGENCY_ADMIN") != "true" {
		return errors.New("DBSPHERE_EMERGENCY_ADMIN must be enabled")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pg, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	return pg.SetEmergencyAdmin(ctx, minutes, cfg.MasterKey)
}
