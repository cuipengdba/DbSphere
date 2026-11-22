package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// CheckMigrationTarget inspects PG access and schema state without creating
// tables or migrating an existing monitoring database.
func CheckMigrationTarget(ctx context.Context, databaseURL string) (serverVersion, schemaVersion int, err error) {
	settings, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return 0, 0, errors.New("DBSPHERE_DATABASE_URL is invalid")
	}
	conn, err := pgx.ConnectConfig(ctx, settings)
	if err != nil {
		return 0, 0, errors.New("cannot connect to the PostgreSQL store; verify the address, TLS and store credentials")
	}
	defer conn.Close(ctx)
	var writable, schemaAccess, initialized bool
	err = conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::integer,
		NOT pg_is_in_recovery() AND current_setting('transaction_read_only') = 'off',
		coalesce(current_schema() = 'public', false) AND has_schema_privilege(current_user, 'public', 'USAGE')
		AND has_schema_privilege(current_user, 'public', 'CREATE'),
		to_regclass('public.dbsphere_schema_migrations') IS NOT NULL`).Scan(&serverVersion, &writable, &schemaAccess, &initialized)
	if err != nil {
		return 0, 0, errors.New("cannot read PostgreSQL version and migration permissions")
	}
	if serverVersion/10000 != 18 {
		return serverVersion, 0, errors.New("DbSphere long-term store requires PostgreSQL 18")
	}
	if !writable || !schemaAccess {
		return serverVersion, 0, errors.New("store migrations require write access and USAGE and CREATE on the active public schema")
	}
	if initialized {
		err = conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM public.dbsphere_schema_migrations`).Scan(&schemaVersion)
		if err != nil {
			return serverVersion, 0, errors.New("cannot read the existing DbSphere schema version with the configured store role")
		}
	}
	return serverVersion, schemaVersion, nil
}
