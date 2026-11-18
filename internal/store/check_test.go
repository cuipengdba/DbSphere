package store

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresConnectionSQLStatesRemainOneAvailabilityIncident(t *testing.T) {
	for _, code := range []string{"08006", "53300", "57P01", "57P02", "57P03"} {
		group, _ := collectionError(&pgconn.PgError{Code: code})
		if group != "availability" {
			t.Errorf("SQLSTATE %s was classified as %s", code, group)
		}
	}
	for _, code := range []string{"28P01", "42501", "42601"} {
		group, _ := collectionError(&pgconn.PgError{Code: code})
		if group != "telemetry" {
			t.Errorf("SQLSTATE %s was classified as %s", code, group)
		}
	}
}
