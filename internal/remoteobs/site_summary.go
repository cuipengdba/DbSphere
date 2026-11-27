package remoteobs

import (
	"regexp"
	"time"
)

const SiteSummaryPath = "/v1/site-summary"
const MaxSiteSummaryBody = 64 << 10
const MaxSiteAssets = 10000
const MaxSiteEvents = 50

var siteRelease = regexp.MustCompile(`^4\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$`)

func ValidSiteRelease(version string) bool { return siteRelease.MatchString(version) }

type SiteEvent struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"startedAt"`
	Status    string    `json:"status"`
}

type SiteSummary struct {
	ID              string      `json:"id"`
	Version         string      `json:"version"`
	ObservedAt      time.Time   `json:"observedAt"`
	IntervalSeconds int         `json:"intervalSeconds"`
	AssetCount      int         `json:"assetCount"`
	Healthy         int         `json:"healthy"`
	Failed          int         `json:"failed"`
	Missing         int         `json:"missing"`
	OpenEvents      int         `json:"openEvents"`
	ResolvedEvents  int         `json:"resolvedEventsLast24h"`
	SignedReports   int         `json:"signedReportsLast30d"`
	Events          []SiteEvent `json:"events"`
}

func (s SiteSummary) Validate(now time.Time) error {
	if !ValidID(s.ID) || !ValidSiteRelease(s.Version) || s.ObservedAt.After(now.Add(15*time.Second)) ||
		s.ObservedAt.Before(now.Add(-MaxReplayAge)) || s.IntervalSeconds < int(MinInterval/time.Second) || s.IntervalSeconds > int(MaxInterval/time.Second) ||
		s.AssetCount < 0 || s.AssetCount > MaxSiteAssets || s.Healthy < 0 || s.Failed < 0 || s.Missing < 0 ||
		s.Healthy > s.AssetCount || s.Failed > s.AssetCount || s.Missing > s.AssetCount ||
		s.Healthy+s.Failed+s.Missing != s.AssetCount || s.OpenEvents < 0 || s.OpenEvents > 10000000 ||
		s.ResolvedEvents < 0 || s.ResolvedEvents > 10000000 || s.SignedReports < 0 || s.SignedReports > 10000000 || len(s.Events) > MaxSiteEvents {
		return ErrInvalid
	}
	seen := make(map[string]bool, len(s.Events))
	for _, event := range s.Events {
		if !ValidID(event.ID) || seen[event.ID] || event.StartedAt.IsZero() || event.StartedAt.After(s.ObservedAt) ||
			(event.Status != "open" && event.Status != "resolved") {
			return ErrInvalid
		}
		seen[event.ID] = true
	}
	return nil
}
