package httpapi

import (
	"strings"
	"testing"
)

func TestOIDCGroupsRejectAmbiguousAndUnscopedGrants(t *testing.T) {
	const raw = `[
		{"group":"Ops","organizationId":"00000000-0000-4000-8000-000000000001","role":"operator"},
		{"group":"DBA","organizationId":"00000000-0000-4000-8000-000000000001","role":"operator"},
		{"group":"Other","organizationId":"00000000-0000-4000-8000-000000000002","role":"admin"}
	]`
	policy, err := parseOIDCGroupPolicy(raw, "https://identity.example.test", "dbsphere")
	if err != nil {
		t.Fatal(err)
	}
	if grant := policy.grant([]string{"DBA", "Ops"}); grant == nil || grant.Role != "operator" {
		t.Fatal("equivalent grants should select the operator role")
	}
	for _, groups := range [][]string{{"Other", "Ops"}, {"ops"}, {"Ops", "\n"}, nil} {
		if policy.grant(groups) != nil {
			t.Errorf("unexpected grant for %q", groups)
		}
	}
	for _, invalid := range []string{strings.ReplaceAll(raw, "DBA", "Ops"), strings.ReplaceAll(raw, "organizationId", "wrong"), raw + " {}"} {
		if _, err := parseOIDCGroupPolicy(invalid, "issuer", "client"); err == nil {
			t.Fatal("invalid deployment policy was accepted")
		}
	}
	config := &AuthConfig{MasterKey: []byte("test-master-key"), OIDC: &OIDCProvider{groupPolicy: policy}}
	token := groupSessionPrefix + strings.Repeat("A", 43)
	oldDigest, ok := accessSessionDigest(config, token)
	if !ok {
		t.Fatal("valid mapped session was rejected")
	}
	config.OIDC.groupPolicy, err = parseOIDCGroupPolicy(raw, "https://identity.example.test", "replacement-client")
	if err != nil {
		t.Fatal(err)
	}
	if digest, ok := accessSessionDigest(config, token); !ok || digest == oldDigest {
		t.Fatal("changing OIDC client should invalidate mapped sessions")
	}
	config.OIDC = nil
	if _, ok := accessSessionDigest(config, token); ok {
		t.Fatal("mapped sessions must expire when mapping is disabled")
	}
}
