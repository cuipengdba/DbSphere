package assets

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,255}$`)
var shortHostPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,150}$`)

type OptionalPort int

func (p *OptionalPort) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || bytes.Equal(data, []byte(`""`)) {
		*p = 0
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		port, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		*p = OptionalPort(port)
		return nil
	}
	var port int
	if err := json.Unmarshal(data, &port); err != nil {
		return err
	}
	*p = OptionalPort(port)
	return nil
}

type Input struct {
	Name              string       `json:"name"`
	Host              string       `json:"host"`
	Port              int          `json:"port"`
	Database          string       `json:"database"`
	Username          string       `json:"username"`
	Password          string       `json:"password"`
	SSLMode           string       `json:"sslMode"`
	Runtime           string       `json:"runtime"`
	LinuxHost         string       `json:"linuxHost"`
	LinuxExporterPort OptionalPort `json:"linuxExporterPort"`
	PoolerURL         string       `json:"poolerUrl"`
	PoolerClear       bool         `json:"poolerClear"`
	K8sCluster        string       `json:"k8sCluster"`
	Namespace         string       `json:"namespace"`
	Workload          string       `json:"workload"`
	Pod               string       `json:"pod"`
	Node              string       `json:"node"`
}

func (a Input) Validate() error {
	if strings.TrimSpace(a.Name) == "" || len(a.Name) > 100 || !hostPattern.MatchString(a.Host) ||
		a.Port < 1 || a.Port > 65535 || a.Database == "" || len(a.Database) > 100 ||
		a.Username == "" || len(a.Username) > 100 || len(a.Password) > 1024 {
		return errors.New("invalid PostgreSQL asset fields")
	}
	if a.SSLMode != "disable" && a.SSLMode != "require" {
		return errors.New("invalid PostgreSQL TLS mode")
	}
	if a.Runtime != "linux" && a.Runtime != "k8s" {
		return errors.New("invalid runtime")
	}
	if a.Runtime == "k8s" && (a.K8sCluster == "" || a.Namespace == "") {
		return errors.New("Kubernetes cluster and namespace are required")
	}
	if a.LinuxExporterPort != 0 && (a.Runtime != "linux" || !shortHostPattern.MatchString(a.LinuxHost) ||
		int(a.LinuxExporterPort) < 1 || int(a.LinuxExporterPort) > 65535) {
		return errors.New("invalid Linux exporter endpoint")
	}
	for _, value := range []string{a.LinuxHost, a.K8sCluster, a.Namespace, a.Workload, a.Pod, a.Node} {
		if len(value) > 150 {
			return errors.New("asset metadata is too long")
		}
	}
	if a.PoolerURL != "" {
		if len(a.PoolerURL) > 2048 {
			return errors.New("PgBouncer URL is too long")
		}
		parsed, err := url.Parse(a.PoolerURL)
		if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") ||
			parsed.Path != "/pgbouncer" || parsed.User == nil || parsed.User.Username() == "" || parsed.Hostname() == "" {
			return errors.New("invalid PgBouncer management URL")
		}
	}
	return nil
}
