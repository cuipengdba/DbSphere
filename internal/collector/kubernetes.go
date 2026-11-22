package collector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type KubernetesTarget struct {
	Cluster   string
	Namespace string
	Pod       string
	Workload  string
}

// KubernetesPodIdentity is a bounded snapshot of a Pod's API-verified identity.
// PVC names come from Pod spec references; states are separate API observations.
type KubernetesPodIdentity struct {
	Namespace string               `json:"namespace"`
	Name      string               `json:"name"`
	UID       string               `json:"uid"`
	CreatedAt time.Time            `json:"createdAt"`
	Node      string               `json:"node,omitempty"`
	PVCs      []string             `json:"pvcs"`
	PVCStates []KubernetesPVCState `json:"pvcStates"`
}

// KubernetesPVCState records a namespaced, read-only GET made while verifying
// an application Pod. An unavailable phase must never be interpreted as Bound.
type KubernetesPVCState struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Phase      string `json:"phase,omitempty"`
	VolumeName string `json:"volumeName,omitempty"`
}

type kubeHTTPError int

func (e kubeHTTPError) Error() string { return fmt.Sprintf("Kubernetes API HTTP %d", int(e)) }

type kubeConfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server   string `yaml:"server"`
			CAData   string `yaml:"certificate-authority-data"`
			CAFile   string `yaml:"certificate-authority"`
			Insecure bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			TokenFile             string `yaml:"tokenFile"`
			Exec                  any    `yaml:"exec"`
			AuthProvider          any    `yaml:"auth-provider"`
			ClientCertificate     string `yaml:"client-certificate"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			Username              string `yaml:"username"`
			Password              string `yaml:"password"`
		} `yaml:"user"`
	} `yaml:"users"`
}

type kubeClient struct {
	base   *url.URL
	client *http.Client
	token  string
}

var kubeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
var kubeUID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// VerifyKubernetesPodIdentity reads one Pod with the configured read-only
// credentials and rejects a reused name whose UID differs from the OTLP value.
func VerifyKubernetesPodIdentity(ctx context.Context, target KubernetesTarget, expectedUID string) (KubernetesPodIdentity, error) {
	if !kubeName.MatchString(target.Namespace) || len(target.Namespace) > 63 ||
		!kubeName.MatchString(target.Pod) || len(target.Pod) > 253 || !kubeUID.MatchString(expectedUID) {
		return KubernetesPodIdentity{}, errors.New("invalid application Pod identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	k, err := kubeCredentials(target)
	if err != nil {
		return KubernetesPodIdentity{}, err
	}
	pod, err := k.get(ctx, "/api/v1/namespaces/"+url.PathEscape(target.Namespace)+
		"/pods/"+url.PathEscape(target.Pod), nil)
	if err != nil {
		return KubernetesPodIdentity{}, err
	}
	metadata := kubeObject(pod["metadata"])
	if kubeString(metadata["name"]) != target.Pod || kubeString(metadata["uid"]) != expectedUID ||
		kubeString(metadata["namespace"]) != target.Namespace {
		return KubernetesPodIdentity{}, errors.New("application Pod UID or namespace does not match Kubernetes API")
	}
	createdAt, err := time.Parse(time.RFC3339, kubeString(metadata["creationTimestamp"]))
	if err != nil {
		return KubernetesPodIdentity{}, errors.New("application Pod creation time is unavailable")
	}
	spec := kubeObject(pod["spec"])
	identity := KubernetesPodIdentity{Namespace: target.Namespace, Name: target.Pod,
		UID: expectedUID, CreatedAt: createdAt.UTC(), PVCs: []string{}, PVCStates: []KubernetesPVCState{}}
	if node := kubeString(spec["nodeName"]); node != "" && kubeName.MatchString(node) && len(node) <= 253 {
		identity.Node = node
	}
	seenPVCs := map[string]bool{}
	for _, raw := range kubeList(spec["volumes"]) {
		name := kubeString(kubeObject(kubeObject(raw)["persistentVolumeClaim"])["claimName"])
		if name == "" || seenPVCs[name] || !kubeName.MatchString(name) || len(name) > 253 {
			continue
		}
		if len(identity.PVCs) >= 10 {
			return KubernetesPodIdentity{}, errors.New("application Pod references more than 10 PVCs")
		}
		seenPVCs[name] = true
		identity.PVCs = append(identity.PVCs, name)
	}
	sort.Strings(identity.PVCs)
	for _, name := range identity.PVCs {
		state := KubernetesPVCState{Name: name, Status: "unavailable"}
		pvc, err := k.get(ctx, "/api/v1/namespaces/"+url.PathEscape(target.Namespace)+
			"/persistentvolumeclaims/"+url.PathEscape(name), nil)
		if err == nil {
			meta := kubeObject(pvc["metadata"])
			if kubeString(meta["namespace"]) == target.Namespace && kubeString(meta["name"]) == name {
				state.Status = "ok"
				switch kubeString(kubeObject(pvc["status"])["phase"]) {
				case "Bound", "Pending", "Lost":
					state.Phase = kubeString(kubeObject(pvc["status"])["phase"])
				default:
					state.Phase = "Unknown"
				}
				if volume := kubeString(kubeObject(pvc["spec"])["volumeName"]); volume != "" &&
					len(volume) <= 253 && kubeName.MatchString(volume) {
					state.VolumeName = volume
				}
			}
		}
		identity.PVCStates = append(identity.PVCStates, state)
	}
	return identity, nil
}

func kubeCredentials(target KubernetesTarget) (*kubeClient, error) {
	configPath := os.Getenv("DBSPHERE_KUBECONFIG")
	server, token, ca := "", "", []byte(nil)
	if configPath != "" {
		contents, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("read Kubernetes config: %w", err)
		}
		var config kubeConfig
		if err := yaml.Unmarshal(contents, &config); err != nil {
			return nil, fmt.Errorf("parse Kubernetes config: %w", err)
		}
		contextName := target.Cluster
		if contextName == "" {
			contextName = config.CurrentContext
		}
		clusterName, userName := "", ""
		for _, item := range config.Contexts {
			if item.Name == contextName {
				clusterName, userName = item.Context.Cluster, item.Context.User
				break
			}
		}
		if clusterName == "" || userName == "" {
			return nil, fmt.Errorf("Kubernetes context %q is not configured", contextName)
		}
		for _, item := range config.Clusters {
			if item.Name != clusterName {
				continue
			}
			if item.Cluster.Insecure {
				return nil, errors.New("Kubernetes TLS verification cannot be disabled")
			}
			server = item.Cluster.Server
			if item.Cluster.CAData != "" {
				var err error
				ca, err = base64.StdEncoding.DecodeString(item.Cluster.CAData)
				if err != nil {
					return nil, fmt.Errorf("decode Kubernetes CA: %w", err)
				}
			} else if item.Cluster.CAFile != "" {
				path := item.Cluster.CAFile
				if !filepath.IsAbs(path) {
					path = filepath.Join(filepath.Dir(configPath), path)
				}
				var err error
				ca, err = os.ReadFile(path)
				if err != nil {
					return nil, fmt.Errorf("read Kubernetes CA: %w", err)
				}
			}
			break
		}
		for _, item := range config.Users {
			if item.Name != userName {
				continue
			}
			if item.User.Exec != nil || item.User.AuthProvider != nil || item.User.ClientCertificate != "" ||
				item.User.ClientCertificateData != "" || item.User.Username != "" || item.User.Password != "" {
				return nil, errors.New("Kubernetes config requires a static read-only bearer token; credential helpers are forbidden")
			}
			token = item.User.Token
			if token == "" && item.User.TokenFile != "" {
				path := item.User.TokenFile
				if !filepath.IsAbs(path) {
					path = filepath.Join(filepath.Dir(configPath), path)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return nil, fmt.Errorf("read Kubernetes token: %w", err)
				}
				token = strings.TrimSpace(string(data))
			}
			break
		}
	} else if host := os.Getenv("KUBERNETES_SERVICE_HOST"); host != "" {
		if net.ParseIP(host) == nil && !kubeName.MatchString(host) {
			return nil, errors.New("invalid Kubernetes Service host")
		}
		port := os.Getenv("KUBERNETES_SERVICE_PORT")
		if port == "" {
			port = "443"
		}
		portNum, err := strconv.Atoi(port)
		if err != nil || portNum < 1 || portNum > 65535 {
			return nil, errors.New("invalid Kubernetes Service port")
		}
		server = "https://" + net.JoinHostPort(host, port)
		ca, err = os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
		if err != nil {
			return nil, fmt.Errorf("read ServiceAccount CA: %w", err)
		}
		secret, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err != nil {
			return nil, fmt.Errorf("read ServiceAccount token: %w", err)
		}
		token = strings.TrimSpace(string(secret))
	} else {
		return nil, errors.New("Kubernetes credentials are not configured")
	}
	parsed, err := url.Parse(server)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || token == "" {
		return nil, errors.New("invalid Kubernetes API endpoint or bearer token")
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(ca) != 0 && !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid Kubernetes CA certificate")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &kubeClient{base: parsed, token: token, client: &http.Client{Transport: transport, Timeout: 4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (k *kubeClient) get(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	endpoint := *k.base
	endpoint.Path = path
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+k.token)
	request.Header.Set("Accept", "application/json")
	response, err := k.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, kubeHTTPError(response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1024*1024 {
		return nil, errors.New("Kubernetes response exceeds 1 MiB")
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func kubeObject(value any) map[string]any { result, _ := value.(map[string]any); return result }
func kubeList(value any) []any            { result, _ := value.([]any); return result }
func kubeString(value any) string         { result, _ := value.(string); return result }
func kubeShort(value any) string {
	text := kubeString(value)
	if len(text) > 300 {
		return text[:300]
	}
	return text
}

// Kubernetes continue tokens keep a list snapshot stable across pages. Bound both
// request count and total objects so discovery cannot consume unbounded memory.
func (k *kubeClient) listBounded(ctx context.Context, path string) ([]any, error) {
	const pageSize, maxPages = 100, 10
	items := make([]any, 0, pageSize)
	seenTokens := map[string]bool{}
	token := ""
	for page := 0; page < maxPages; page++ {
		query := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if token != "" {
			query.Set("continue", token)
		}
		result, err := k.get(ctx, path, query)
		if err != nil {
			return nil, err
		}
		pageItems := kubeList(result["items"])
		if len(pageItems) > pageSize || len(items)+len(pageItems) > pageSize*maxPages {
			return nil, errors.New("Kubernetes list exceeds 1000 items; mapping is uncertain")
		}
		items = append(items, pageItems...)
		next := kubeString(kubeObject(result["metadata"])["continue"])
		if next == "" {
			return items, nil
		}
		if seenTokens[next] {
			return nil, errors.New("Kubernetes list repeated a continue token; mapping is uncertain")
		}
		seenTokens[next] = true
		token = next
	}
	return nil, errors.New("Kubernetes list exceeds 10 pages; mapping is uncertain")
}

var errKubernetesWorkloadNotFound = errors.New("Kubernetes Workload was not found for Pod mapping")

func (k *kubeClient) resolveWorkloadPod(ctx context.Context, namespace, workload, preferred string) (map[string]any, map[string]any, error) {
	base := "/apis/apps/v1/namespaces/" + url.PathEscape(namespace)
	var selector map[string]any
	var identity map[string]any
	workloadUID, workloadKind := "", ""
	for _, resource := range []string{"deployments", "statefulsets"} {
		items, err := k.listBounded(ctx, base+"/"+resource)
		if err != nil {
			return nil, nil, fmt.Errorf("read Kubernetes Workload list: %w", err)
		}
		for _, raw := range items {
			item := kubeObject(raw)
			if kubeString(kubeObject(item["metadata"])["name"]) != workload {
				continue
			}
			if selector != nil {
				return nil, nil, errors.New("Kubernetes Workload name is ambiguous")
			}
			workloadUID = kubeString(kubeObject(item["metadata"])["uid"])
			if workloadUID == "" {
				return nil, nil, errors.New("Kubernetes Workload UID is missing")
			}
			workloadKind = resource
			kind := "Deployment"
			if resource == "statefulsets" {
				kind = "StatefulSet"
			}
			identity = map[string]any{"kind": kind, "name": workload, "uid": workloadUID}
			specSelector := kubeObject(kubeObject(item["spec"])["selector"])
			if len(kubeList(specSelector["matchExpressions"])) != 0 {
				return nil, nil, errors.New("Kubernetes Workload selector expressions are not supported")
			}
			selector = kubeObject(specSelector["matchLabels"])
			if len(selector) == 0 {
				return nil, nil, errors.New("Kubernetes Workload has no exact Pod labels")
			}
		}
	}
	if selector == nil {
		return nil, nil, errKubernetesWorkloadNotFound
	}
	replicaSetUIDs := map[string]bool{}
	if workloadKind == "deployments" {
		sets, err := k.listBounded(ctx, base+"/replicasets")
		if err != nil {
			return nil, nil, fmt.Errorf("read Kubernetes ReplicaSets for Workload: %w", err)
		}
		for _, raw := range sets {
			metadata := kubeObject(kubeObject(raw)["metadata"])
			setUID := kubeString(metadata["uid"])
			if setUID == "" {
				continue
			}
			for _, reference := range kubeList(metadata["ownerReferences"]) {
				owner := kubeObject(reference)
				if kubeString(owner["kind"]) == "Deployment" && kubeString(owner["uid"]) == workloadUID && owner["controller"] != false {
					replicaSetUIDs[setUID] = true
				}
			}
		}
	}
	pods, err := k.listBounded(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods")
	if err != nil {
		return nil, nil, fmt.Errorf("read Kubernetes Pods for Workload: %w", err)
	}
	all, active, ready := []map[string]any{}, []map[string]any{}, []map[string]any{}
	preferredSeen := false
	for _, raw := range pods {
		pod := kubeObject(raw)
		metadata := kubeObject(pod["metadata"])
		name := kubeString(metadata["name"])
		if preferred != "" && name == preferred {
			preferredSeen = true
		}
		if !kubeName.MatchString(name) || !kubeUID.MatchString(kubeString(metadata["uid"])) {
			continue
		}
		labels := kubeObject(metadata["labels"])
		matched := true
		for key, value := range selector {
			if labels[key] != value {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		owned := false
		for _, reference := range kubeList(metadata["ownerReferences"]) {
			owner := kubeObject(reference)
			if owner["controller"] == false {
				continue
			}
			if workloadKind == "statefulsets" && kubeString(owner["kind"]) == "StatefulSet" && kubeString(owner["uid"]) == workloadUID {
				owned = true
			}
			if workloadKind == "deployments" && kubeString(owner["kind"]) == "ReplicaSet" && replicaSetUIDs[kubeString(owner["uid"])] {
				owned = true
			}
		}
		if !owned {
			continue
		}
		if name == preferred {
			return pod, identity, nil
		}
		all = append(all, pod)
		if metadata["deletionTimestamp"] != nil {
			continue
		}
		active = append(active, pod)
		for _, rawCondition := range kubeList(kubeObject(pod["status"])["conditions"]) {
			condition := kubeObject(rawCondition)
			if kubeString(condition["type"]) == "Ready" && kubeString(condition["status"]) == "True" {
				ready = append(ready, pod)
				break
			}
		}
	}
	if preferredSeen {
		return nil, nil, errors.New("Configured Kubernetes Pod could not be verified against the Workload UID")
	}
	if len(ready) == 1 {
		return ready[0], identity, nil
	}
	if len(ready) > 1 {
		return nil, nil, errors.New("Kubernetes Workload has multiple ready Pods; mapping is ambiguous")
	}
	if len(active) == 1 {
		return active[0], identity, nil
	}
	if len(all) == 1 {
		return all[0], identity, nil
	}
	if len(all) == 0 {
		return nil, nil, errors.New("Kubernetes Workload has no matching Pod")
	}
	return nil, nil, errors.New("Kubernetes Workload has multiple unready Pods; mapping is ambiguous")
}

func (k *kubeClient) readMappedPod(ctx context.Context, target KubernetesTarget) (map[string]any, map[string]any, error) {
	name, expectedUID := target.Pod, ""
	var workload map[string]any
	if target.Workload != "" {
		selected, identity, err := k.resolveWorkloadPod(ctx, target.Namespace, target.Workload, target.Pod)
		if errors.Is(err, errKubernetesWorkloadNotFound) || errors.Is(err, kubeHTTPError(http.StatusForbidden)) {
			cnpgPod, cnpgIdentity, cnpgErr := k.resolveCNPGPod(ctx, target)
			if cnpgIdentity != nil || cnpgErr == nil {
				selected, identity, err = cnpgPod, cnpgIdentity, cnpgErr
			} else if errors.Is(err, errKubernetesWorkloadNotFound) && !errors.Is(cnpgErr, kubeHTTPError(http.StatusNotFound)) {
				return nil, nil, fmt.Errorf("read CloudNativePG Cluster: %w", cnpgErr)
			}
		}
		if err != nil {
			return nil, identity, err
		}
		metadata := kubeObject(selected["metadata"])
		name, expectedUID, workload = kubeString(metadata["name"]), kubeString(metadata["uid"]), identity
	}
	if !kubeName.MatchString(name) || len(name) > 253 {
		return nil, nil, errors.New("Invalid Kubernetes Pod mapping")
	}
	path := "/api/v1/namespaces/" + url.PathEscape(target.Namespace) + "/pods/" + url.PathEscape(name)
	pod, err := k.get(ctx, path, nil)
	if err != nil {
		return nil, workload, err
	}
	metadata := kubeObject(pod["metadata"])
	uid, namespace := kubeString(metadata["uid"]), kubeString(metadata["namespace"])
	if !kubeUID.MatchString(uid) || metadata["name"] != name ||
		(namespace != "" && namespace != target.Namespace) || (expectedUID != "" && uid != expectedUID) {
		return nil, workload, errors.New("Kubernetes Pod identity changed or could not be verified")
	}
	return pod, workload, nil
}

// Kubernetes reads the mapped Pod and related read-only resources. A configured
// single-replica workload can resolve a replacement Pod after a rollout.
func Kubernetes(ctx context.Context, target KubernetesTarget) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if !kubeName.MatchString(target.Namespace) || len(target.Namespace) > 63 ||
		(target.Workload != "" && (!kubeName.MatchString(target.Workload) || len(target.Workload) > 253)) ||
		(target.Pod != "" && (!kubeName.MatchString(target.Pod) || len(target.Pod) > 253)) {
		return map[string]any{"status": "error", "reason": "Invalid Kubernetes namespace, Workload or Pod mapping"}
	}
	if target.Pod == "" && target.Workload == "" {
		return map[string]any{"status": "unmapped", "reason": "Namespace and Pod or Workload mapping are required"}
	}
	k, err := kubeCredentials(target)
	if err != nil {
		return map[string]any{"status": "not_configured", "reason": err.Error()}
	}
	base := "/api/v1/namespaces/" + url.PathEscape(target.Namespace)
	pod, workload, err := k.readMappedPod(ctx, target)
	operator := kubeObject(workload["operator"])
	delete(workload, "operator")
	if err != nil {
		result := map[string]any{"status": "error", "reason": err.Error(), "cluster": target.Cluster, "namespace": target.Namespace}
		if operator != nil {
			result["workload"], result["cloudNativePG"] = workload, operator
		}
		return result
	}
	metadata, spec, state := kubeObject(pod["metadata"]), kubeObject(pod["spec"]), kubeObject(pod["status"])
	uid, nodeName := kubeString(metadata["uid"]), kubeString(spec["nodeName"])
	podName := kubeString(metadata["name"])
	var scheduledAt time.Time
	conditions := []any{}
	for _, raw := range kubeList(state["conditions"]) {
		item := kubeObject(raw)
		switch kubeString(item["type"]) {
		case "PodScheduled", "Initialized", "ContainersReady", "Ready":
			conditions = append(conditions, map[string]any{"type": item["type"], "status": item["status"],
				"reason": item["reason"], "message": kubeShort(item["message"]), "changedAt": item["lastTransitionTime"]})
			if item["type"] == "PodScheduled" && item["status"] == "True" {
				scheduledAt, _ = time.Parse(time.RFC3339Nano, kubeString(item["lastTransitionTime"]))
			}
		}
	}
	containers := []any{}
	for _, raw := range kubeList(state["containerStatuses"]) {
		item := kubeObject(raw)
		terminated := kubeObject(kubeObject(item["state"])["terminated"])
		if len(terminated) == 0 {
			terminated = kubeObject(kubeObject(item["lastState"])["terminated"])
		}
		containers = append(containers, map[string]any{"name": item["name"], "ready": item["ready"],
			"restartCount": item["restartCount"], "terminatedReason": terminated["reason"],
			"lastTerminatedAt": terminated["finishedAt"], "waitingReason": kubeObject(kubeObject(item["state"])["waiting"])["reason"]})
	}
	result := map[string]any{"status": "ok", "cluster": target.Cluster, "namespace": target.Namespace,
		"pod": map[string]any{"name": metadata["name"], "uid": uid, "phase": state["phase"], "deletingAt": metadata["deletionTimestamp"],
			"reason": state["reason"], "message": kubeShort(state["message"]), "node": nodeName, "createdAt": metadata["creationTimestamp"],
			"conditions": conditions}, "containers": containers, "pvcs": []any{}, "events": []any{},
		"node": nil, "services": []any{}, "resourceMetrics": nil,
		"observedAt": time.Now().UTC()}
	if workload != nil {
		result["workload"] = workload
	}
	if operator != nil {
		result["cloudNativePG"] = operator
	}
	resourceSpecs := map[string]any{}
	for _, raw := range kubeList(spec["containers"]) {
		container := kubeObject(raw)
		name := kubeString(container["name"])
		if name == "" || len(resourceSpecs) >= 32 {
			continue
		}
		values := map[string]any{}
		for _, kind := range []string{"requests", "limits"} {
			resources := kubeObject(kubeObject(container["resources"])[kind])
			var units kubeUsageTotals
			values[kind] = units.add(kubeString(resources["cpu"]), kubeString(resources["memory"]))
		}
		resourceSpecs[name] = values
	}
	result["resourceSpecs"] = resourceSpecs
	for _, raw := range kubeList(metadata["ownerReferences"]) {
		owner := kubeObject(raw)
		if owner["controller"] == true {
			result["owner"] = map[string]any{"kind": owner["kind"], "name": owner["name"], "uid": owner["uid"]}
			break
		}
	}
	if workload != nil {
		result["podMapping"] = map[string]any{"configured": target.Pod, "resolved": podName,
			"workload": target.Workload, "source": "workload_owner_uid"}
	}
	if nodeName != "" && kubeName.MatchString(nodeName) {
		if node, err := k.get(ctx, "/api/v1/nodes/"+url.PathEscape(nodeName), nil); err == nil {
			nodeMeta := kubeObject(node["metadata"])
			createdAt, _ := time.Parse(time.RFC3339Nano, kubeString(nodeMeta["creationTimestamp"]))
			if nodeMeta["name"] != nodeName || !kubeUID.MatchString(kubeString(nodeMeta["uid"])) ||
				(!scheduledAt.IsZero() && createdAt.After(scheduledAt)) {
				result["nodeError"] = "Kubernetes Node identity is inconsistent with Pod scheduling"
			} else {
				info := kubeObject(node["status"])
				result["node"] = map[string]any{"name": nodeName, "uid": nodeMeta["uid"], "conditions": info["conditions"],
					"allocatable": info["allocatable"]}
			}
		} else {
			result["nodeError"] = err.Error()
		}
	}
	pvcs := []any{}
	seen := map[string]bool{}
	for _, raw := range kubeList(spec["volumes"]) {
		name := kubeString(kubeObject(kubeObject(raw)["persistentVolumeClaim"])["claimName"])
		if name == "" || seen[name] || !kubeName.MatchString(name) {
			continue
		}
		if len(pvcs) >= 10 {
			result["pvcError"] = "PVC list truncated at 10 items"
			break
		}
		seen[name] = true
		pvc, err := k.get(ctx, base+"/persistentvolumeclaims/"+url.PathEscape(name), nil)
		if err != nil {
			pvcs = append(pvcs, map[string]any{"name": name, "status": "unavailable", "error": err.Error()})
			continue
		}
		pvcMeta := kubeObject(pvc["metadata"])
		pvcUID := kubeString(pvcMeta["uid"])
		if pvcMeta["name"] != name || pvcMeta["namespace"] != target.Namespace || !kubeUID.MatchString(pvcUID) {
			pvcs = append(pvcs, map[string]any{"name": name, "status": "unavailable", "error": "Kubernetes PVC identity could not be verified"})
			continue
		}
		pvcSpec := kubeObject(pvc["spec"])
		phase := kubeObject(pvc["status"])["phase"]
		entry := map[string]any{"name": name, "uid": pvcUID, "namespace": target.Namespace, "status": "ok",
			"phase": phase, "createdAt": pvcMeta["creationTimestamp"],
			"capacity":  kubeObject(kubeObject(pvc["status"])["capacity"])["storage"],
			"requested": kubeObject(kubeObject(pvcSpec["resources"])["requests"])["storage"],
			"usage":     map[string]any{"status": "unavailable", "reason": "PVC usage requires a bound claim and verified creation time"}}
		if createdAt, err := time.Parse(time.RFC3339Nano, kubeString(pvcMeta["creationTimestamp"])); phase == "Bound" && err == nil {
			entry["usage"] = pvcUsageSince(ctx, target.Namespace, name, createdAt)
		}
		if volumeName := kubeString(pvcSpec["volumeName"]); phase == "Bound" && kubeName.MatchString(volumeName) {
			volume, err := k.get(ctx, "/api/v1/persistentvolumes/"+url.PathEscape(volumeName), nil)
			volumeMeta := kubeObject(volume["metadata"])
			claim := kubeObject(kubeObject(volume["spec"])["claimRef"])
			if err != nil {
				entry["volumeError"] = err.Error()
			} else if volumeMeta["name"] != volumeName || !kubeUID.MatchString(kubeString(volumeMeta["uid"])) ||
				claim["uid"] != pvcUID || claim["name"] != name || claim["namespace"] != target.Namespace {
				entry["volumeError"] = "PersistentVolume claimRef does not match the current PVC identity"
			} else {
				entry["volume"] = map[string]any{"name": volumeName, "uid": volumeMeta["uid"], "claimUID": pvcUID,
					"phase":    kubeObject(volume["status"])["phase"],
					"capacity": kubeObject(kubeObject(volume["spec"])["capacity"])["storage"]}
			}
		}
		pvcs = append(pvcs, entry)
	}
	result["pvcs"] = pvcs
	if serviceList, err := k.get(ctx, base+"/services", url.Values{"limit": {"100"}}); err == nil {
		labels := kubeObject(metadata["labels"])
		services := []any{}
		serviceNames := make(map[string]string)
		for _, raw := range kubeList(serviceList["items"]) {
			service := kubeObject(raw)
			serviceSpec := kubeObject(service["spec"])
			selector := kubeObject(serviceSpec["selector"])
			if len(selector) == 0 {
				continue
			}
			matches := true
			for key, value := range selector {
				if labels[key] != value {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
			ports := []any{}
			for _, rawPort := range kubeList(serviceSpec["ports"]) {
				ports = append(ports, kubeObject(rawPort)["port"])
			}
			serviceMeta := kubeObject(service["metadata"])
			name, serviceUID := kubeString(serviceMeta["name"]), kubeString(serviceMeta["uid"])
			serviceNames[name] = serviceUID
			services = append(services, map[string]any{"name": name, "uid": serviceUID, "ports": ports,
				"type": serviceSpec["type"], "clusterIP": serviceSpec["clusterIP"]})
		}
		result["services"] = services
		if kubeString(kubeObject(serviceList["metadata"])["continue"]) != "" {
			result["servicesError"] = "Service list truncated at 100 items"
		}
		if len(serviceNames) > 0 {
			result["endpointRoutes"] = k.endpointRoutes(ctx, target.Namespace, podName, uid, serviceNames)
		} else if result["servicesError"] != nil {
			result["endpointRoutes"] = map[string]any{"status": "unavailable", "reason": result["servicesError"]}
		} else {
			result["endpointRoutes"] = map[string]any{"status": "not_applicable"}
		}
	} else {
		result["servicesError"] = err.Error()
		result["endpointRoutes"] = map[string]any{"status": "unavailable", "reason": err.Error()}
	}
	if uid != "" {
		query := url.Values{"fieldSelector": {"involvedObject.uid=" + uid}, "limit": {"20"}}
		if events, err := k.get(ctx, base+"/events", query); err == nil {
			items := []any{}
			for _, raw := range kubeList(events["items"]) {
				item := kubeObject(raw)
				if kubeObject(item["involvedObject"])["uid"] != uid {
					continue
				}
				at := item["lastTimestamp"]
				if at == nil {
					at = item["eventTime"]
				}
				if at == nil {
					at = kubeObject(item["metadata"])["creationTimestamp"]
				}
				items = append(items, map[string]any{"id": kubeObject(item["metadata"])["uid"], "objectUID": uid,
					"reason": item["reason"], "message": kubeShort(item["message"]), "at": at})
			}
			result["events"] = items
		} else {
			result["eventsError"] = err.Error()
		}
	}
	metricPath := "/apis/metrics.k8s.io/v1beta1"
	if podMetrics, err := k.get(ctx, metricPath+"/namespaces/"+url.PathEscape(target.Namespace)+
		"/pods/"+url.PathEscape(podName), nil); err == nil {
		usage := []any{}
		totals := kubeUsageTotals{}
		for _, raw := range kubeList(podMetrics["containers"]) {
			container := kubeObject(raw)
			resources := kubeObject(container["usage"])
			value := totals.add(kubeString(resources["cpu"]), kubeString(resources["memory"]))
			value["name"], value["cpu"], value["memory"] = container["name"], resources["cpu"], resources["memory"]
			usage = append(usage, value)
		}
		result["resourceMetrics"] = map[string]any{"source": "metrics.k8s.io",
			"observedAt": podMetrics["timestamp"], "window": podMetrics["window"], "containers": usage, "node": nil}
		metricUID := kubeString(kubeObject(podMetrics["metadata"])["uid"])
		created, createdErr := time.Parse(time.RFC3339Nano, kubeString(metadata["creationTimestamp"]))
		observed, observedErr := time.Parse(time.RFC3339Nano, kubeString(podMetrics["timestamp"]))
		if createdErr == nil && observedErr == nil && (metricUID == "" || metricUID == uid) && !observed.Before(created) {
			kubeObject(result["resourceMetrics"])["podUsage"] = totals.values()
		} else {
			result["resourceMetricsError"] = "Pod metrics identity or observation timestamp could not be verified"
			kubeObject(result["resourceMetrics"])["containers"] = []any{}
		}
	} else {
		result["resourceMetricsError"] = err.Error()
	}
	if result["node"] != nil {
		if nodeMetrics, err := k.get(ctx, metricPath+"/nodes/"+url.PathEscape(nodeName), nil); err == nil {
			resources := kubeObject(nodeMetrics["usage"])
			if metrics := kubeObject(result["resourceMetrics"]); len(metrics) != 0 {
				metrics["node"] = map[string]any{"cpu": resources["cpu"], "memory": resources["memory"]}
				if metrics["observedAt"] == nil {
					metrics["observedAt"] = nodeMetrics["timestamp"]
				}
			}
		} else {
			result["nodeMetricsError"] = err.Error()
		}
	}
	return result
}

func (k *kubeClient) endpointRoutes(ctx context.Context, namespace, podName, podUID string, services map[string]string) map[string]any {
	path := "/apis/discovery.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/endpointslices"
	list, err := k.get(ctx, path, url.Values{"limit": {"100"}})
	if err != nil {
		return map[string]any{"status": "unavailable", "reason": err.Error()}
	}
	routes := map[string]any{}
	mismatches := 0
	for _, raw := range kubeList(list["items"]) {
		slice := kubeObject(raw)
		metadata := kubeObject(slice["metadata"])
		service := kubeString(kubeObject(metadata["labels"])["kubernetes.io/service-name"])
		serviceUID, selected := services[service]
		if !selected {
			continue
		}
		stale := false
		for _, rawOwner := range kubeList(metadata["ownerReferences"]) {
			owner := kubeObject(rawOwner)
			if owner["kind"] == "Service" && (owner["name"] != service || owner["uid"] != serviceUID) {
				stale = true
				break
			}
		}
		if stale {
			mismatches++
			continue
		}
		key := kubeString(metadata["name"])
		endpoints := map[string]any{}
		for _, rawEndpoint := range kubeList(slice["endpoints"]) {
			point := kubeObject(rawEndpoint)
			ref := kubeObject(point["targetRef"])
			if ref["kind"] != "Pod" || (ref["namespace"] != nil && ref["namespace"] != namespace) {
				continue
			}
			if ref["name"] == podName && kubeString(ref["uid"]) != podUID {
				mismatches++
			}
			if podUID != "" && kubeString(ref["uid"]) == podUID && (ref["name"] == nil || ref["name"] == podName) {
				endpoints = map[string]any{"addresses": point["addresses"], "conditions": point["conditions"], "targetUID": podUID}
				break
			}
		}
		if len(endpoints) != 0 && len(routes) < 32 {
			routes[key] = map[string]any{"uid": metadata["uid"], "service": service, "serviceUID": serviceUID, "addressType": slice["addressType"],
				"ports": slice["ports"], "endpoint": endpoints}
		}
	}
	return map[string]any{"status": "ok", "slices": routes, "identityMismatches": mismatches,
		"truncated": len(routes) == 32 || kubeString(kubeObject(list["metadata"])["continue"]) != ""}
}

// DiscoverAssetPod previews identity using the configured read-only Kubernetes access.
func DiscoverAssetPod(ctx context.Context, target KubernetesTarget, port int) map[string]any {
	unknown := map[string]any{"status": "unavailable", "namespace": target.Namespace, "workload": target.Workload, "port": port}
	if !kubeName.MatchString(target.Namespace) || len(target.Namespace) > 63 {
		return unknown
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client, err := kubeCredentials(target)
	if err != nil {
		return unknown
	}
	pod, workload, err := client.readMappedPod(ctx, target)
	if workload != nil {
		unknown["workloadIdentity"] = workload
	}
	if err != nil {
		return unknown
	}
	metadata := kubeObject(pod["metadata"])
	if metadata["namespace"] != target.Namespace {
		return unknown
	}
	unknown["portDeclared"] = false
	for _, item := range kubeList(kubeObject(pod["spec"])["containers"]) {
		for _, raw := range kubeList(kubeObject(item)["ports"]) {
			if kubeObject(raw)["containerPort"] == float64(port) {
				unknown["portDeclared"] = true
			}
		}
	}
	unknown["status"], unknown["pod"], unknown["uid"] = "observed", metadata["name"], metadata["uid"]
	return unknown
}
