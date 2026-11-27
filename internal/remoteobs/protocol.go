// Package remoteobs contains the wire format for site collectors.
package remoteobs

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
)

const (
	Version      = 1
	Path         = "/v1/observations"
	MaxBody      = 512 << 10
	MaxReplayAge = 72 * time.Hour
	MinInterval  = 10 * time.Second
	MaxInterval  = time.Hour
)

var (
	assetIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sitePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	ErrInvalid     = errors.New("invalid remote observation")
)

type Observation struct {
	Version    int             `json:"version"`
	ID         string          `json:"id"`
	AssetID    string          `json:"assetId"`
	Engine     string          `json:"engine"`
	ObservedAt time.Time       `json:"observedAt"`
	Failure    string          `json:"failure,omitempty"`
	Metrics    json.RawMessage `json:"metrics,omitempty"`
	Backlog    int             `json:"backlog"`
}

type Receipt struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func ValidID(id string) bool     { return assetIDPattern.MatchString(id) }
func ValidSite(site string) bool { return sitePattern.MatchString(site) }

func ValidEngine(engine string) bool {
	return engine == "postgresql" || engine == "mysql" || engine == "redis"
}

func (item Observation) Validate(now time.Time) error {
	if item.Version != Version || !ValidID(item.ID) || !ValidID(item.AssetID) || !ValidEngine(item.Engine) ||
		item.ObservedAt.Before(now.Add(-MaxReplayAge)) || item.ObservedAt.After(now.Add(15*time.Second)) ||
		item.Backlog < 0 || item.Backlog > 10000 {
		return ErrInvalid
	}
	if item.Failure != "" {
		if (item.Failure != "target_unavailable" && item.Failure != "credential_unavailable") || len(item.Metrics) != 0 {
			return ErrInvalid
		}
		return nil
	}
	if len(item.Metrics) < 2 || len(item.Metrics) > MaxBody-2048 || item.Metrics[0] != '{' || !json.Valid(item.Metrics) {
		return ErrInvalid
	}
	return nil
}

func Decode(reader io.Reader, value any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrInvalid
	}
	return nil
}
