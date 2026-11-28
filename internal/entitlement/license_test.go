package entitlement

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

func TestSignatureAndValidity(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	license := License{Deployment: "00000000-0000-4000-8000-000000000001", Sequence: 2,
		IssuedAt: now, StartsAt: now, ExpiresAt: now.Add(time.Hour), Assets: 40, Sites: 2}
	wire, err := json.Marshal(license)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Issue(wire, private)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Verify(signed, public)
	if err != nil || decoded != license {
		t.Fatalf("unexpected verification result: %+v %v", decoded, err)
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		last time.Time
	}{{"active", now, now.Add(ClockTolerance)}, {"clock_skew", now, now.Add(ClockTolerance + time.Second)},
		{"pending", now.Add(-time.Second), time.Time{}}, {"expired", license.ExpiresAt, now}} {
		if got := decoded.Status(tc.at, tc.last); got != tc.name {
			t.Fatalf("expected %s, got %s", tc.name, got)
		}
	}
	var envelope signedLicense
	if err := json.Unmarshal(signed, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Signature = "AAAA"
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tampered, public); err == nil {
		t.Fatal("tampered signature accepted")
	}
}
