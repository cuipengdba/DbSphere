package assets

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var ErrInvalidImport = errors.New("invalid asset manifest")

type ImportRequest struct {
	Format        string `json:"format"`
	Content       string `json:"content"`
	Revision      string `json:"revision,omitempty"`
	BatchID       string `json:"batchId,omitempty"`
	CredentialRef string `json:"credentialRef,omitempty"`
	Username      string `json:"username,omitempty"`
	Discover      bool   `json:"discover,omitempty"`
}

type Declaration struct {
	Input
	ID            string `json:"id,omitempty"`
	CredentialRef string `json:"credentialRef,omitempty"`
	Issue         string `json:"-"`
}

func ParseImport(request ImportRequest) ([]Declaration, error) {
	if len(request.Content) == 0 || len(request.Content) > 256<<10 {
		return nil, ErrInvalidImport
	}
	var declarations []Declaration
	var err error
	switch request.Format {
	case "csv":
		declarations, err = csvDeclarations(request.Content)
	case "yaml", "pigsty":
		dec := yaml.NewDecoder(strings.NewReader(request.Content))
		var node, extra yaml.Node
		if dec.Decode(&node) != nil || dec.Decode(&extra) != io.EOF || !boundedNode(&node, 0, new(int)) {
			return nil, ErrInvalidImport
		}
		if request.Format == "pigsty" {
			declarations = pigstyDeclarations(&node)
		} else {
			var document struct{ Assets []map[string]any }
			if node.Decode(&document) != nil {
				return nil, ErrInvalidImport
			}
			for _, row := range document.Assets {
				declarations = append(declarations, decodeDeclaration(row))
			}
		}
	default:
		return nil, ErrInvalidImport
	}
	if err != nil || len(declarations) == 0 || len(declarations) > 100 {
		return nil, ErrInvalidImport
	}
	for i := range declarations {
		item := &declarations[i]
		item.ID = strings.ToLower(strings.TrimSpace(item.ID))
		item.Name, item.Host = strings.TrimSpace(item.Name), strings.TrimSpace(item.Host)
		if item.Runtime == "" {
			item.Runtime = "managed"
		}
		if item.Port == 0 {
			item.Port = map[string]int{RuntimeMySQL: 3306, RuntimeRedis: 6379, RuntimeEtcd: 2379, RuntimeDM8: 5236, RuntimeKingbase: 54321}[item.Runtime]
			if item.Port == 0 {
				item.Port = 5432
			}
		}
		if item.Database == "" {
			switch item.Runtime {
			case RuntimeRedis:
				item.Database = "0"
			case RuntimeEtcd:
				item.Database = "etcd"
			case RuntimeDM8:
				item.Database = "DAMENG"
			case RuntimeMySQL:
				item.Database = "mysql"
			default:
				item.Database = "postgres"
			}
		}
		if item.Username == "" {
			item.Username = request.Username
		}
		if item.SSLMode == "" {
			item.SSLMode = "require"
			if item.Runtime == RuntimeDM8 {
				item.SSLMode = "disable"
			}
		}
		if item.CredentialRef == "" || request.CredentialRef != "" && item.ID != "" && item.CredentialRef == "asset:"+item.ID {
			item.CredentialRef = request.CredentialRef
		}
	}
	return declarations, nil
}

func boundedNode(node *yaml.Node, depth int, count *int) bool {
	*count = *count + 1
	if depth > 20 || *count > 18000 || node.Kind == yaml.AliasNode {
		return false
	}
	for _, child := range node.Content {
		if !boundedNode(child, depth+1, count) {
			return false
		}
	}
	return true
}

func decodeDeclaration(row map[string]any) Declaration {
	var item Declaration
	raw, err := json.Marshal(row)
	if err != nil {
		item.Issue = "资产字段格式无效"
		return item
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&item) != nil {
		item.Issue = "存在未知字段或值格式无效"
	}
	if item.Password != "" || item.PoolerURL != "" || item.PatroniURL != "" || item.PoolerClear || item.PatroniClear {
		item.Issue = "文件中的凭据须使用引用"
	}
	return item
}

func csvDeclarations(content string) ([]Declaration, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(content, "\uFEFF")))
	headers, err := reader.Read()
	if err != nil || len(headers) < 2 || len(headers) > 24 {
		return nil, ErrInvalidImport
	}
	seen := map[string]bool{}
	for i, field := range headers {
		headers[i] = strings.TrimSpace(field)
		if headers[i] == "" || seen[headers[i]] {
			return nil, ErrInvalidImport
		}
		seen[headers[i]] = true
	}
	rows := []Declaration{}
	for {
		values, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rows) >= 100 {
			return nil, ErrInvalidImport
		}
		row := map[string]any{}
		for i, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			switch headers[i] {
			case "port", "linuxExporterPort":
				if number, err := strconv.Atoi(value); err == nil {
					row[headers[i]] = number
				} else {
					row[headers[i]] = false
				}
			case "tags":
				row[headers[i]] = strings.Split(value, "|")
			default:
				row[headers[i]] = value
			}
		}
		rows = append(rows, decodeDeclaration(row))
	}
	return rows, nil
}

type pigstyVars struct {
	Cluster     string `yaml:"pg_cluster"`
	Port        int    `yaml:"pg_port"`
	Sequence    int    `yaml:"pg_seq"`
	Role        string `yaml:"pg_role"`
	AnsibleHost string `yaml:"ansible_host"`
}

type pigstyGroup struct {
	Vars     pigstyVars
	Hosts    map[string]pigstyVars
	Children map[string]pigstyGroup
}

func pigstyDeclarations(root *yaml.Node) []Declaration {
	var groups map[string]pigstyGroup
	if root.Decode(&groups) != nil {
		return nil
	}
	all := groups["all"]
	if len(all.Children) > 0 {
		groups = all.Children
	}
	rows := []Declaration{}
	for _, group := range sortedKeys(groups) {
		section := groups[group]
		cluster := section.Vars.Cluster
		if cluster == "" {
			continue
		}
		for _, address := range sortedKeys(section.Hosts) {
			host := section.Hosts[address]
			port := host.Port
			if port == 0 {
				port = section.Vars.Port
			}
			if port == 0 {
				port = all.Vars.Port
			}
			name := cluster + "-" + address
			if host.Sequence > 0 {
				name = fmt.Sprintf("%s-%d", cluster, host.Sequence)
			}
			if host.AnsibleHost != "" {
				address = host.AnsibleHost
			}
			item := Declaration{Input: Input{Name: name, Host: address, Port: port, ClusterName: &cluster, Runtime: "linux"}}
			if host.Role != "" {
				item.Tags = &[]string{"pg_role=" + host.Role}
			}
			rows = append(rows, item)
		}
	}
	return rows
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
