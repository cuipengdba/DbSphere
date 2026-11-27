package store

import (
	"testing"
	"time"
)

func TestChangePreflightRequiresObservedLockSignal(t *testing.T) {
	for _, test := range []struct {
		name, sample, status, risk string
		waits                      *float64
	}{
		{"PostgreSQL clean", `{"adapter":{"transport":{"readOnly":true}},"connections":2,"diagnostics":{"locks":{"status":"ok","data":{"blockedSessions":0}}}}`, "ok", "ready", changeFloat(0)},
		{"MySQL missing lock view", `{"adapter":{"engine":"mysql","transport":{"readOnly":true},"signals":[{"key":"lockWaits","status":"permission_denied"}]},"mysql":{"connections":2,"lockWaits":[]}}`, "ok", "limited", nil},
		{"MySQL wait present", `{"adapter":{"engine":"mysql","transport":{"readOnly":true},"signals":[{"key":"lockWaits","status":"ok"}]},"mysql":{"connections":2,"lockWaits":[{}]}}`, "ok", "lock_wait", changeFloat(1)},
		{"collection failure", `{}`, "error", "failed", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			point, err := changeSnapshotFromSample("sample", time.Now(), test.status, []byte(test.sample))
			if err != nil || point.Risk != test.risk || (point.LockWaits == nil) != (test.waits == nil) ||
				(point.LockWaits != nil && *point.LockWaits != *test.waits) {
				t.Fatalf("unexpected preflight %+v: %v", point, err)
			}
		})
	}
}

func changeFloat(value float64) *float64 { return &value }
