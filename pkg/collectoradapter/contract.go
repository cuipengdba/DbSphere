// Package collectoradapter defines the observation contract used by DbSphere
// database collectors. A collector uses an independently supplied credential
// and returns a read-only snapshot of one target.
package collectoradapter

import "context"

const ContractVersion = 1

type Target struct {
	Host              string
	Port              int
	Database          string
	Username          string
	SSLMode           string
	Runtime           string
	KubernetesCluster string
}

type Signal struct {
	Key    string `json:"key"`
	Source string `json:"source"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type Transport struct {
	Encrypted bool   `json:"encrypted"`
	Verified  bool   `json:"verified"`
	Protocol  string `json:"protocol,omitempty"`
	ReadOnly  bool   `json:"readOnly"`
}

type Report struct {
	Contract       int       `json:"contract"`
	Engine         string    `json:"engine"`
	Product        string    `json:"product,omitempty"`
	Profile        string    `json:"profile"`
	ProviderStatus string    `json:"providerStatus,omitempty"`
	Version        int       `json:"version"`
	Role           string    `json:"role"`
	Transport      Transport `json:"transport"`
	Signals        []Signal  `json:"signals"`
}

type Snapshot struct {
	Metrics map[string]any
	Report  Report
}

type Adapter interface {
	Collect(ctx context.Context, target Target, password string) (Snapshot, error)
}
