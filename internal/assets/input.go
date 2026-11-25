package assets

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,255}$`)
var shortHostPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,150}$`)
var poolerHostPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

// RuntimeMySQL reserves a separate collection route for this preview. Older
// DbSphere versions skip these assets when they run against the same store.
const RuntimeMySQL = "mysql"
const RuntimeDM8 = "dm8"
const RuntimeKingbase = "kingbase"

func IsPostgreSQLRuntime(runtime string) bool {
	return runtime == "linux" || runtime == "k8s" || runtime == "managed"
}

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
	OrganizationID    string       `json:"organizationId"`
	Name              string       `json:"name"`
	ClusterName       *string      `json:"clusterName"`
	Environment       *string      `json:"environment"`
	Tags              *[]string    `json:"tags"`
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
	PoolerEndpoint    string       `json:"poolerEndpoint"`
	PatroniURL        string       `json:"patroniUrl"`
	PatroniClear      bool         `json:"patroniClear"`
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
		return errors.New("invalid database asset fields")
	}
	if a.ClusterName != nil && (len(*a.ClusterName) > 150 || strings.ContainsAny(*a.ClusterName, "\x00\r\n\t")) {
		return errors.New("invalid PostgreSQL cluster name")
	}
	if a.Environment != nil && (len(*a.Environment) > 80 || strings.ContainsAny(*a.Environment, "\x00\r\n\t")) {
		return errors.New("invalid asset environment")
	}
	if a.Tags != nil {
		if len(*a.Tags) > 12 {
			return errors.New("too many asset tags")
		}
		for _, tag := range *a.Tags {
			if len(tag) > 64 || strings.TrimSpace(tag) == "" || strings.ContainsAny(tag, "\x00\r\n\t,，") {
				return errors.New("invalid asset tag")
			}
		}
	}
	if a.SSLMode != "disable" && a.SSLMode != "require" {
		return errors.New("invalid database TLS mode")
	}
	if !IsPostgreSQLRuntime(a.Runtime) && a.Runtime != RuntimeMySQL && a.Runtime != RuntimeDM8 && a.Runtime != RuntimeKingbase {
		return errors.New("invalid runtime")
	}
	if !IsPostgreSQLRuntime(a.Runtime) {
		if a.Runtime == RuntimeDM8 && a.SSLMode != "disable" {
			return errors.New("DM8 uses an HTTPS probe with a local database connection")
		}
		if a.LinuxHost != "" || a.LinuxExporterPort != 0 || a.K8sCluster != "" || a.Namespace != "" ||
			a.Workload != "" || a.Pod != "" || a.Node != "" || a.PoolerURL != "" || a.PoolerEndpoint != "" || a.PatroniURL != "" {
			return errors.New("database engine does not support PostgreSQL infrastructure mappings")
		}
		return nil
	}
	if a.Runtime == "managed" && (a.LinuxHost != "" || a.LinuxExporterPort != 0 ||
		a.K8sCluster != "" || a.Namespace != "" || a.Workload != "" || a.Pod != "" || a.Node != "") {
		return errors.New("managed PostgreSQL cannot have Linux or Kubernetes resource mapping")
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
	if a.PoolerEndpoint != "" {
		if _, _, err := ParsePoolerEndpoint(a.PoolerEndpoint); err != nil {
			return err
		}
	}
	if a.PatroniURL != "" {
		if len(a.PatroniURL) > 2048 {
			return errors.New("Patroni REST URL is too long")
		}
		parsed, err := url.Parse(a.PatroniURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Hostname() == "" || (parsed.Path != "" && parsed.Path != "/") ||
			parsed.RawQuery != "" || parsed.Fragment != "" ||
			(parsed.User != nil && parsed.User.Username() == "") {
			return errors.New("invalid Patroni REST URL")
		}
	}
	return nil
}

// MonitoringClusterName normalizes the optional grouping label. A nil input
// on update means the caller did not send the field.
func (a Input) MonitoringClusterName() *string {
	if a.ClusterName == nil {
		return nil
	}
	name := strings.TrimSpace(*a.ClusterName)
	return &name
}

func (a Input) MonitoringEnvironment() *string {
	if a.Environment == nil {
		return nil
	}
	value := strings.TrimSpace(*a.Environment)
	return &value
}

func (a Input) MonitoringTags() []string {
	if a.Tags == nil {
		return nil
	}
	value := make([]string, len(*a.Tags))
	for i, tag := range *a.Tags {
		value[i] = strings.TrimSpace(tag)
	}
	slices.Sort(value)
	return slices.Compact(value)
}

// ParsePoolerEndpoint accepts only a host and TCP port. It never interprets
// credentials, URL paths or query parameters as an application route.
func ParsePoolerEndpoint(value string) (string, int, error) {
	if len(value) > 261 {
		return "", 0, errors.New("PgBouncer application endpoint is too long")
	}
	host, rawPort, err := net.SplitHostPort(value)
	if err != nil || host == "" || len(host) > 255 ||
		(net.ParseIP(host) == nil && !poolerHostPattern.MatchString(host)) ||
		rawPort == "" || len(rawPort) > 5 || strings.Trim(rawPort, "0123456789") != "" {
		return "", 0, errors.New("invalid PgBouncer application endpoint")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("invalid PgBouncer application port")
	}
	return host, port, nil
}
