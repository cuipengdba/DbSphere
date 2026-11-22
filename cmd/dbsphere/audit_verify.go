package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/cuipengdba/dbsphere/internal/security"
)

func verifyAuditFile(args []string) error {
	if len(args) < 1 || len(args) > 3 {
		return errors.New("usage: dbsphere verify-audit FILE [KEY_FINGERPRINT] [PREVIOUS_HEAD_HASH]")
	}
	file, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer file.Close()
	var export security.AuditExport
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&export); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("extra data after the audit export")
	}
	keyFingerprint := ""
	if len(args) >= 2 {
		keyFingerprint = args[1]
	}
	if err := security.VerifyAuditExport(export, keyFingerprint); err != nil {
		return err
	}
	if len(args) == 3 && !slices.ContainsFunc(export.Entries, func(item security.AuditEntry) bool { return item.Hash == args[2] }) {
		return errors.New("previous audit checkpoint is absent")
	}
	fmt.Printf("verified organization=%s entries=%d fingerprint=%s head=%s\n", export.OrganizationID, export.HeadSequence, export.KeyFingerprint, export.HeadHash)
	return nil
}
