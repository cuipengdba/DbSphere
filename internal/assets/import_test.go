package assets

import (
	"errors"
	"testing"
)

func TestImportFormatsAndCredentialReferences(t *testing.T) {
	tests := []struct {
		format  string
		content string
		host    string
		port    int
	}{
		{"csv", "name,host,port,tags\norder,db.local,5434,prod|east", "db.local", 5434},
		{"yaml", "assets: [{name: order, host: db.local}]", "db.local", 5432},
		{"pigsty", "all: {vars: {pg_port: 6432}, children: {order: {vars: {pg_cluster: order}, hosts: {db: {ansible_host: db.local, pg_seq: 1, pg_role: primary}}}}}", "db.local", 6432},
	}
	for _, scenario := range tests {
		t.Run(scenario.format, func(t *testing.T) {
			items, err := ParseImport(ImportRequest{Format: scenario.format, Content: scenario.content, Username: "monitor", CredentialRef: "env:DBSPHERE_IMPORT_ORDERS"})
			if err != nil || len(items) != 1 {
				t.Fatalf("parse import: %v", err)
			}
			item := items[0]
			if item.Issue != "" || item.Host != scenario.host || item.Port != scenario.port || item.Database != "postgres" ||
				item.SSLMode != "require" || item.CredentialRef != "env:DBSPHERE_IMPORT_ORDERS" || item.Input.Validate() != nil {
				t.Fatalf("unexpected import declaration: %+v", item)
			}
		})
	}
}

func TestImportRejectsEmbeddedCredentialsAndAliases(t *testing.T) {
	items, err := ParseImport(ImportRequest{Format: "yaml", Content: "assets: [{name: order, host: db.local, password: cleartext}]"})
	if err != nil || len(items) != 1 || items[0].Issue == "" {
		t.Fatalf("cleartext was not rejected: %v", err)
	}
	_, err = ParseImport(ImportRequest{Format: "yaml", Content: "item: &item {name: order}\nassets: [*item]"})
	if !errors.Is(err, ErrInvalidImport) {
		t.Fatal("accepted YAML alias")
	}
}
