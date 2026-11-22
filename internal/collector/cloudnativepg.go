package collector

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"time"
)

const cnpgAPI = "/apis/postgresql.cnpg.io/v1/namespaces/"

func cnpgClusterPath(namespace string) string {
	return cnpgAPI + url.PathEscape(namespace) + "/clusters"
}

// Keep operator evidence limited to the status fields the console displays.
func cnpgClusterStatus(cluster map[string]any, namespace string) map[string]any {
	meta, spec, status := kubeObject(cluster["metadata"]), kubeObject(cluster["spec"]), kubeObject(cluster["status"])
	name, uid := kubeString(meta["name"]), kubeString(meta["uid"])
	if !kubeName.MatchString(name) || !kubeUID.MatchString(uid) || meta["namespace"] != namespace {
		return nil
	}
	conditions := []any{}
	for _, raw := range kubeList(status["conditions"]) {
		condition := kubeObject(raw)
		conditions = append(conditions, map[string]any{"type": condition["type"], "status": condition["status"],
			"reason": condition["reason"], "message": kubeShort(condition["message"]), "changedAt": condition["lastTransitionTime"]})
		if len(conditions) == 20 {
			break
		}
	}
	backup := map[string]any{"status": "unknown"}
	for _, key := range []string{"firstRecoverabilityPoint", "lastSuccessfulBackup", "lastFailedBackup"} {
		if timestamp, err := time.Parse(time.RFC3339Nano, kubeString(status[key])); err == nil {
			backup[key] = timestamp.UTC()
			backup["status"] = "observed"
		}
	}
	return map[string]any{
		"name": name, "namespace": namespace, "uid": uid, "phase": status["phase"], "phaseReason": kubeShort(status["phaseReason"]),
		"currentPrimary": status["currentPrimary"], "targetPrimary": status["targetPrimary"], "instanceCount": spec["instances"],
		"readyInstances": status["readyInstances"], "readWriteService": status["writeService"], "readService": status["readService"],
		"image": status["image"], "conditions": conditions, "backup": backup,
	}
}

func (k *kubeClient) resolveCNPGPod(ctx context.Context, target KubernetesTarget) (map[string]any, map[string]any, error) {
	cluster, err := k.get(ctx, cnpgClusterPath(target.Namespace)+"/"+url.PathEscape(target.Workload), nil)
	if err != nil {
		return nil, nil, err
	}
	operator := cnpgClusterStatus(cluster, target.Namespace)
	if operator == nil || operator["name"] != target.Workload {
		return nil, nil, errors.New("CloudNativePG Cluster identity could not be verified")
	}
	identity := map[string]any{"kind": "Cluster", "name": operator["name"], "uid": operator["uid"], "operator": operator}
	objects, err := k.listBounded(ctx, "/api/v1/namespaces/"+url.PathEscape(target.Namespace)+"/pods")
	if err != nil {
		return nil, identity, err
	}
	choose := target.Pod
	if choose == "" {
		choose = kubeString(operator["currentPrimary"])
	}
	members := []any{}
	var chosen, only map[string]any
	for _, raw := range objects {
		pod := kubeObject(raw)
		meta := kubeObject(pod["metadata"])
		name, uid := kubeString(meta["name"]), kubeString(meta["uid"])
		labels := kubeObject(meta["labels"])
		if labels["cnpg.io/cluster"] != target.Workload || labels["cnpg.io/instanceName"] != name ||
			meta["namespace"] != target.Namespace || !kubeUID.MatchString(uid) {
			continue
		}
		owned := false
		for _, rawOwner := range kubeList(meta["ownerReferences"]) {
			owner := kubeObject(rawOwner)
			if owner["kind"] == "Cluster" && owner["apiVersion"] == "postgresql.cnpg.io/v1" && owner["uid"] == operator["uid"] {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		state := kubeObject(pod["status"])
		member := map[string]any{"name": name, "uid": uid, "phase": state["phase"], "roleLabel": labels["cnpg.io/instanceRole"],
			"address": state["podIP"], "deleting": meta["deletionTimestamp"] != nil}
		for _, rawCondition := range kubeList(state["conditions"]) {
			condition := kubeObject(rawCondition)
			if condition["type"] == "Ready" {
				member["ready"] = condition["status"]
				break
			}
		}
		members = append(members, member)
		only = pod
		if name == choose {
			chosen = pod
		}
	}
	operator["members"] = members
	if chosen == nil && choose == "" && len(members) == 1 {
		chosen = only
	}
	if chosen == nil {
		return nil, identity, errors.New("CloudNativePG Cluster has no verified Pod for this mapping")
	}
	return chosen, identity, nil
}

// Compare the operator report with the backend identified by PostgreSQL itself.
func CompareCNPGRole(kube, postgres map[string]any) {
	operator := kubeObject(kube["cloudNativePG"])
	if operator == nil {
		return
	}
	role := kubeString(postgres["role"])
	observation := map[string]any{"status": "unverified", "pgRole": role}
	operator["databaseObservation"] = observation
	address, err := netip.ParseAddr(kubeString(kubeObject(postgres["postgresEndpoint"])["address"]))
	primary := kubeString(operator["currentPrimary"])
	if err != nil || (role != "primary" && role != "standby") || !kubeName.MatchString(primary) {
		return
	}
	var match map[string]any
	for _, raw := range kubeList(operator["members"]) {
		member := kubeObject(raw)
		ip, err := netip.ParseAddr(kubeString(member["address"]))
		if err != nil || ip.Unmap() != address.Unmap() {
			continue
		}
		if match != nil {
			return
		}
		match = member
	}
	if match == nil {
		return
	}
	controllerRole := "standby"
	if match["name"] == primary {
		controllerRole = "primary"
	}
	state := "aligned"
	if role != controllerRole {
		state = "mismatch"
	}
	operator["databaseObservation"] = map[string]any{"status": state, "pgRole": role, "operatorRole": controllerRole,
		"pod": match["name"], "podUID": match["uid"], "address": address.String(),
		"matchesMappedPod": kubeObject(kube["pod"])["uid"] == match["uid"]}
}

// PreviewCloudNativePG lists bounded, read-only candidates without applying assets.
func PreviewCloudNativePG(ctx context.Context, target KubernetesTarget) map[string]any {
	if !kubeName.MatchString(target.Namespace) || len(target.Namespace) > 63 || len(target.Cluster) > 253 {
		return map[string]any{"status": "invalid", "reason": "Invalid Kubernetes context or namespace"}
	}
	client, err := kubeCredentials(target)
	if err != nil {
		return map[string]any{"status": "unavailable", "reason": err.Error()}
	}
	list, err := client.get(ctx, cnpgClusterPath(target.Namespace), url.Values{"limit": {"100"}})
	if err != nil {
		return map[string]any{"status": "unavailable", "reason": err.Error()}
	}
	clusters := []any{}
	for _, raw := range kubeList(list["items"]) {
		if observed := cnpgClusterStatus(kubeObject(raw), target.Namespace); observed != nil {
			clusters = append(clusters, observed)
		}
	}
	return map[string]any{"status": "ok", "clusters": clusters, "truncated": kubeString(kubeObject(list["metadata"])["continue"]) != ""}
}
