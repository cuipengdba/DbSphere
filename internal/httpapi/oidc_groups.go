package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

const oidcGroupSessionLifetime = 15 * time.Minute

type oidcGroupMapping struct {
	Group string `json:"group"`
	storepkg.OIDCGroupGrant
}

type oidcGroupPolicy struct {
	grantByGroup map[string]storepkg.OIDCGroupGrant
	fingerprint  string
}

type oidcGroupStore interface {
	ApplyOIDCGroupGrant(context.Context, string, string, time.Time, *storepkg.OIDCGroupGrant) (storepkg.AccessAccount, error)
}

var oidcScopeName = regexp.MustCompile(`^[a-zA-Z0-9_:-]{1,64}$`)

func parseOIDCGroupPolicy(raw, issuer, clientID string) (*oidcGroupPolicy, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 32*1024 {
		return nil, errors.New("OIDC group mappings exceed 32 KiB")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var entries []oidcGroupMapping
	if dec.Decode(&entries) != nil || dec.Decode(new(any)) != io.EOF || len(entries) == 0 || len(entries) > 100 {
		return nil, errors.New("OIDC group mappings must be a JSON array with 1 to 100 entries")
	}
	groups := make(map[string]storepkg.OIDCGroupGrant, len(entries))
	for _, entry := range entries {
		if !validOIDCGroup(entry.Group) || !entry.OIDCGroupGrant.Valid() {
			return nil, errors.New("invalid OIDC group, organization, role or team")
		}
		if _, exists := groups[entry.Group]; exists {
			return nil, errors.New("duplicate OIDC group mapping")
		}
		slices.Sort(entry.TeamIDs)
		if len(entry.TeamIDs) == 0 {
			entry.TeamIDs = nil
		}
		groups[entry.Group] = entry.OIDCGroupGrant
	}
	canonical, err := json.Marshal(struct {
		Issuer, Client string
		Grants         map[string]storepkg.OIDCGroupGrant
	}{issuer, clientID, groups})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	return &oidcGroupPolicy{grantByGroup: groups, fingerprint: hex.EncodeToString(digest[:])}, nil
}

func validOIDCGroup(value string) bool {
	return value != "" && len(value) <= 255 && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// Only unambiguous, organization-scoped grants are applied. Equivalent grants
// can match multiple groups without depending on the claim's ordering.
func (policy *oidcGroupPolicy) grant(claims []string) *storepkg.OIDCGroupGrant {
	if len(claims) > 256 {
		return nil
	}
	var grant *storepkg.OIDCGroupGrant
	for _, group := range claims {
		if !validOIDCGroup(group) {
			return nil
		}
		candidate, exists := policy.grantByGroup[group]
		if !exists {
			continue
		}
		if grant != nil && (candidate.OrganizationID != grant.OrganizationID || candidate.Role != grant.Role || !slices.Equal(candidate.TeamIDs, grant.TeamIDs)) {
			return nil
		}
		grant = &candidate
	}
	return grant
}
