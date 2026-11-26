package store

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

type requestInfrastructure struct {
	AppState string                 `json:"appState"`
	AppPod   *requestPodIdentity    `json:"appPod,omitempty"`
	Changes  []string               `json:"changes,omitempty"`
	Samples  []infrastructureSample `json:"samples"`
}

type requestPodIdentity struct {
	collector.KubernetesPodIdentity
	Cluster    string    `json:"cluster,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

type infrastructureSample struct {
	Layer    string    `json:"layer"`
	SampleID string    `json:"sampleId"`
	At       time.Time `json:"at"`
	Identity string    `json:"identity,omitempty"`
	Relation string    `json:"relation"`
}

func requestPodEvidence(span requestSpanObservation) (string, *requestPodIdentity) {
	var reported struct{ Namespace, Name, UID string }
	if json.Unmarshal(span.ReportedPod, &reported) != nil || reported.UID == "" {
		return "unreported", nil
	}
	var verified requestPodIdentity
	if span.TimingAnomaly || json.Unmarshal(span.VerifiedPod, &verified) != nil || verified.UID != reported.UID ||
		verified.Namespace == "" || verified.Name == "" || verified.Namespace != reported.Namespace || verified.Name != reported.Name ||
		verified.CreatedAt.IsZero() || verified.CreatedAt.After(span.Start) || verified.ObservedAt.Before(verified.CreatedAt) {
		return "reported", nil
	}
	return "verified", &verified
}

func inRequestWindow(at time.Time, span requestSpanObservation) bool {
	return !at.Before(span.Start) && !at.After(span.End)
}

func sourceObservationTime(source map[string]any, fallback time.Time) time.Time {
	if source["observedAt"] == nil {
		return fallback
	}
	return storageTime(source["observedAt"])
}

func collectRequestInfrastructure(span requestSpanObservation, ordered []sampleObservation) requestInfrastructure {
	state, pod := requestPodEvidence(span)
	result := requestInfrastructure{AppState: state, AppPod: pod, Samples: []infrastructureSample{}}
	if span.TimingAnomaly {
		return result
	}
	positions := map[string]int{}
	for _, sample := range ordered {
		if !inRequestWindow(sample.At, span) {
			continue
		}
		add := func(layer, identity, relation string, observedAt time.Time) {
			if !inRequestWindow(observedAt, span) {
				return
			}
			evidence := infrastructureSample{layer, sample.ID, observedAt, identity, relation}
			if index, exists := positions[layer]; exists {
				old := result.Samples[index]
				if old.Relation == relation && observedAt.After(old.At) || old.Relation == "same_window" && relation != "same_window" {
					result.Samples[index] = evidence
				}
				return
			}
			positions[layer] = len(result.Samples)
			result.Samples = append(result.Samples, evidence)
		}
		linux := object(sample.Metrics["linux"])
		if linux["status"] == "ok" {
			add("linux", stringValue(linux["endpoint"]), "same_window", sourceObservationTime(linux, sample.At))
		}
		kube := object(sample.Metrics["k8s"])
		podUID := stringValue(object(kube["pod"])["uid"])
		if kube["status"] == "ok" && podUID != "" {
			relation := "same_window"
			if pod != nil && pod.UID == podUID && kube["namespace"] == pod.Namespace && object(kube["pod"])["name"] == pod.Name {
				relation = "same_pod_uid"
			}
			observedAt := sourceObservationTime(kube, sample.At)
			add("kubernetes", podUID, relation, observedAt)
			for _, raw := range list(kube["pvcs"]) {
				claim := object(raw)
				claimUID := stringValue(claim["uid"])
				if claim["status"] != "ok" || claimUID == "" {
					continue
				}
				claimRelation := "same_window"
				if pod != nil && claim["namespace"] == pod.Namespace {
					for _, appClaim := range pod.PVCStates {
						if appClaim.Name == claim["name"] && appClaim.ClaimUID == claimUID && appClaim.CreatedAt != nil && !appClaim.CreatedAt.After(span.Start) {
							claimRelation = "same_claim_uid"
							break
						}
					}
				}
				add("volume", stringValue(claim["name"])+" · "+claimUID, claimRelation, observedAt)
			}
		}
		storage := object(sample.Metrics["containerStorage"])
		if storage["status"] == "ok" {
			container := object(object(storage["containers"])[stringValue(storage["primary"])])
			if container["status"] == "ok" {
				runtimeID := stringValue(container["id"])
				relation := "same_window"
				if pod != nil && pod.UID == storage["podUID"] && pod.Namespace == storage["namespace"] {
					for _, appContainer := range pod.Containers {
						if runtimeID != "" && appContainer.RuntimeID == runtimeID && !appContainer.StartedAt.After(span.Start) {
							relation = "same_runtime_id"
							break
						}
					}
				}
				add("container", runtimeID, relation, sourceObservationTime(container, sample.At))
			}
		}
	}
	return result
}

// Compare chronological API observations for the same mapped workload. A
// reused name is only a lookup key, never a substitute for a UID.
func recordRequestIdentityChanges(paths []requestPathEvidence) {
	for index := range paths {
		current := &paths[index].Infrastructure
		if current.AppPod == nil || current.AppPod.Cluster == "" {
			continue
		}
		var nearest *requestPodIdentity
		for _, previous := range paths {
			pod := previous.Infrastructure.AppPod
			if pod == nil || pod.Cluster != current.AppPod.Cluster || pod.Namespace != current.AppPod.Namespace || pod.Name != current.AppPod.Name ||
				!pod.ObservedAt.Before(current.AppPod.ObservedAt) {
				continue
			}
			if nearest == nil || pod.ObservedAt.After(nearest.ObservedAt) {
				nearest = pod
			}
		}
		if nearest == nil {
			continue
		}
		if nearest.UID != current.AppPod.UID {
			addRequestChange(current, "pod_replaced")
		}
		for _, observed := range current.AppPod.PVCStates {
			for _, prior := range nearest.PVCStates {
				if observed.Name != prior.Name {
					continue
				}
				if observed.ClaimUID != "" && prior.ClaimUID != "" && observed.ClaimUID != prior.ClaimUID {
					addRequestChange(current, "pvc_replaced:"+observed.Name)
				}
				if observed.VolumeName != "" && observed.VolumeName == prior.VolumeName &&
					observed.VolumeUID != "" && prior.VolumeUID != "" && observed.VolumeUID != prior.VolumeUID {
					addRequestChange(current, "volume_replaced:"+observed.VolumeName)
				}
			}
		}
	}
}

func addRequestChange(infrastructure *requestInfrastructure, change string) {
	if len(infrastructure.Changes) >= 20 {
		return
	}
	if slices.Contains(infrastructure.Changes, change) {
		return
	}
	infrastructure.Changes = append(infrastructure.Changes, change)
}
