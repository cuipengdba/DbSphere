package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var backupName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
var backupAssetID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var walName = regexp.MustCompile(`^[0-9A-F]{24}$`)

type backupEnvelope struct {
	PgBackRest []backupStanza   `json:"pgbackrest"`
	Attempt    backupAttempt    `json:"lastAttempt"`
	WAL        []backupWALCheck `json:"walChecks"`
	Restores   []backupRestore  `json:"restoreTests"`
	RPO        int              `json:"targetRpoSeconds,omitempty"`
	RTO        int              `json:"targetRtoSeconds,omitempty"`
}

type backupAttempt struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

type backupStanza struct {
	Name   string                     `json:"name"`
	Status struct{ Code json.Number } `json:"status"`
	Repo   []struct {
		Key    int
		Status struct{ Code json.Number }
	} `json:"repo"`
	Archive []struct {
		Database backupDatabase
		Min      string
		Max      string
	} `json:"archive"`
	Backup []backupJob `json:"backup"`
}

type backupJob struct {
	Label, Type, Prior string
	Reference          []string
	Error              bool
	Database           backupDatabase
	Timestamp          struct{ Start, Stop int64 }
	Archive            struct{ Start, Stop string }
	Info               struct{ Size, Delta int64 }
}

type backupDatabase struct {
	ID  int `json:"id"`
	Key int `json:"repo-key"`
}

type backupWALCheck struct {
	RepoKey      int      `json:"repoKey"`
	DatabaseID   int      `json:"databaseId"`
	From         string   `json:"from"`
	To           string   `json:"to"`
	SegmentBytes uint64   `json:"segmentBytes"`
	Segments     []string `json:"segments"`
}

type backupRestore struct {
	RepoKey    int    `json:"repoKey"`
	DatabaseID int    `json:"databaseId,omitempty"`
	Backup     string `json:"backup"`
	recoveryWitness
}

type recoveryWitness struct {
	ID               string    `json:"id,omitempty"`
	TargetID         string    `json:"targetId,omitempty"`
	Isolated         bool      `json:"isolated,omitempty"`
	BeforeMarker     string    `json:"beforeMarker,omitempty"`
	AfterMarker      string    `json:"afterMarker,omitempty"`
	TargetAt         time.Time `json:"targetAt"`
	StartedAt        time.Time `json:"startedAt"`
	FinishedAt       time.Time `json:"finishedAt"`
	SourceCommitAt   time.Time `json:"sourceCommitAt"`
	RestoredCommitAt time.Time `json:"restoredCommitAt"`
	AfterCommitAt    time.Time `json:"afterCommitAt,omitempty"`
	ConfirmedBefore  bool      `json:"confirmedBefore"`
	ExcludedAfter    bool      `json:"excludedAfter"`
}

// Backup reads an operator-exported report within the configured read-only root.
func Backup(assetID string, now time.Time) map[string]any {
	directory := os.Getenv("DBSPHERE_BACKUP_INFO_DIR")
	if directory == "" {
		return nil
	}
	result := map[string]any{"status": "unavailable", "source": "pgBackRest info", "reason": "备份清单缺失、无权限或内容无效"}
	if !backupAssetID.MatchString(assetID) {
		return result
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return result
	}
	defer root.Close()
	name := assetID + ".json"
	if info, err := root.Stat(name); err != nil || !info.Mode().IsRegular() {
		return result
	}
	file, err := root.Open(name)
	if err != nil {
		return result
	}
	defer file.Close()
	data, err := boundedBackupFile(file, 1<<20)
	if err != nil {
		return result
	}
	if parsed, ok := readBackupCatalog(data, now); ok {
		return parsed
	}
	return result
}

func boundedBackupFile(file *os.File, maximum int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("invalid backup report file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("invalid backup report size")
	}
	return content, nil
}

func readBackupCatalog(data []byte, now time.Time) (map[string]any, bool) {
	var envelope backupEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		if err := json.Unmarshal(data, &envelope.PgBackRest); err != nil {
			return nil, false
		}
	}
	if len(envelope.PgBackRest) != 1 {
		return nil, false
	}
	stanza := envelope.PgBackRest[0]
	code, err := stanza.Status.Code.Int64()
	if err != nil || code < 0 || !backupName.MatchString(stanza.Name) {
		return nil, false
	}
	catalog := map[string]any{"status": "ok", "source": "pgBackRest info", "stanza": stanza.Name,
		"repositoryHealthy": code == 0, "repositoryCode": code}
	if !envelope.Attempt.At.IsZero() && !envelope.Attempt.At.After(now.Add(5*time.Minute)) &&
		(envelope.Attempt.Status == "success" || envelope.Attempt.Status == "failed") {
		catalog["lastAttemptStatus"] = envelope.Attempt.Status
		catalog["lastAttemptAt"] = envelope.Attempt.At
	}
	repositories := []map[string]any{}
	for _, repo := range stanza.Repo {
		if repoCode, err := repo.Status.Code.Int64(); err == nil && repo.Key > 0 && len(repositories) < 16 {
			repositories = append(repositories, map[string]any{"key": repo.Key, "healthy": repoCode == 0, "code": repoCode})
		}
	}
	archives := []map[string]any{}
	archiveKeys := map[backupDatabase]bool{}
	for _, archive := range stanza.Archive {
		if archive.Database.Key > 0 && len(archives) < 32 {
			archiveKeys[archive.Database] = true
			archives = append(archives, map[string]any{"repoKey": archive.Database.Key, "databaseId": archive.Database.ID,
				"first": validWAL(archive.Min), "last": validWAL(archive.Max)})
		}
	}
	labels := map[backupDatabase]map[string]backupJob{}
	var lastSuccess int64
	jobs := make([]map[string]any, 0, min(len(stanza.Backup), 100))
	for i := len(stanza.Backup) - 1; i >= 0; i-- {
		job := stanza.Backup[i]
		key, start, stop := job.Database.Key, job.Timestamp.Start, job.Timestamp.Stop
		if key < 0 || start <= 0 || stop < start || stop > now.Add(5*time.Minute).Unix() || !backupName.MatchString(job.Label) ||
			(job.Type != "full" && job.Type != "diff" && job.Type != "incr") {
			continue
		}
		if labels[job.Database] == nil {
			labels[job.Database] = map[string]backupJob{}
		}
		labels[job.Database][job.Label] = job
		if !job.Error && stop > lastSuccess {
			lastSuccess = stop
		}
		if len(jobs) == 100 {
			continue
		}
		refs := make([]string, 0, min(len(job.Reference), 32))
		for _, ref := range job.Reference {
			if backupName.MatchString(ref) && len(refs) < 32 {
				refs = append(refs, ref)
			}
		}
		jobs = append(jobs, map[string]any{"label": job.Label, "type": job.Type, "repoKey": key, "databaseId": job.Database.ID,
			"startedAt": time.Unix(start, 0).UTC(), "completedAt": time.Unix(stop, 0).UTC(), "failed": job.Error,
			"prior": refName(job.Prior), "references": refs, "walStart": validWAL(job.Archive.Start), "walStop": validWAL(job.Archive.Stop),
			"bytes": job.Info.Size, "changedBytes": job.Info.Delta})
	}
	for _, job := range jobs {
		byRepo := labels[backupDatabase{ID: job["databaseId"].(int), Key: job["repoKey"].(int)}]
		chain, complete := recoveryChain(byRepo, job["label"].(string))
		job["chain"] = chain
		job["dependenciesAvailable"] = complete
	}
	if lastSuccess > 0 {
		catalog["lastSuccessAt"] = time.Unix(lastSuccess, 0).UTC()
	}
	catalog["repositories"], catalog["archives"], catalog["jobs"] = repositories, archives, jobs
	catalog["jobLimit"] = len(stanza.Backup) > len(jobs)
	catalog["targetRpoSeconds"], catalog["targetRtoSeconds"] = max(0, envelope.RPO), max(0, envelope.RTO)
	coverage := []map[string]any{}
	for _, check := range envelope.WAL[:min(16, len(envelope.WAL))] {
		if interval, ok := checkedWAL(check); ok && archiveKeys[backupDatabase{ID: check.DatabaseID, Key: check.RepoKey}] {
			coverage = append(coverage, interval)
		}
	}
	restores := []map[string]any{}
	for _, restore := range envelope.Restores[:min(20, len(envelope.Restores))] {
		matches, backupStop, databaseID := 0, int64(0), 0
		var chain []string
		for scope, backups := range labels {
			if scope.Key != restore.RepoKey || restore.DatabaseID > 0 && scope.ID != restore.DatabaseID {
				continue
			}
			job, found := backups[restore.Backup]
			if dependencies, complete := recoveryChain(backups, restore.Backup); found && complete {
				matches++
				backupStop, databaseID, chain = job.Timestamp.Stop, scope.ID, dependencies
			}
		}
		if matches == 1 && restore.TargetAt.Unix() >= backupStop &&
			restore.validTimes(now) {
			proof := restore.projection()
			proof["repoKey"], proof["databaseId"], proof["label"], proof["chain"] = restore.RepoKey, databaseID, restore.Backup, chain
			restores = append(restores, proof)
		}
	}
	catalog["walChecks"], catalog["restoreTests"] = coverage, restores
	return catalog, true
}

func recoveryChain(backups map[string]backupJob, label string) ([]string, bool) {
	chain := []string{}
	state := map[string]uint8{}
	var visit func(string) bool
	visit = func(name string) bool {
		if state[name] == 2 {
			return true
		}
		job, found := backups[name]
		if !found || job.Error || state[name] == 1 || len(state) >= 128 {
			return false
		}
		state[name] = 1
		dependencies := job.Reference
		if job.Prior != "" {
			dependencies = append([]string{job.Prior}, dependencies...)
		}
		if (job.Type == "full") != (len(dependencies) == 0) {
			return false
		}
		for _, dependency := range dependencies {
			if !backupName.MatchString(dependency) || backups[dependency].Timestamp.Stop > job.Timestamp.Start || !visit(dependency) {
				return false
			}
		}
		state[name] = 2
		chain = append(chain, name)
		return true
	}
	complete := visit(label)
	return chain, complete
}

func (r recoveryWitness) validTimes(now time.Time) bool {
	return !r.TargetAt.IsZero() && !r.StartedAt.Before(r.TargetAt) && !r.FinishedAt.Before(r.StartedAt) &&
		!r.FinishedAt.After(now.Add(5*time.Minute))
}

func (r recoveryWitness) verified() bool {
	return backupName.MatchString(r.ID) && r.Isolated && backupName.MatchString(r.BeforeMarker) &&
		backupName.MatchString(r.AfterMarker) && r.BeforeMarker != r.AfterMarker && r.ConfirmedBefore && r.ExcludedAfter &&
		!r.RestoredCommitAt.IsZero() && !r.RestoredCommitAt.After(r.TargetAt) && r.AfterCommitAt.After(r.TargetAt) &&
		!r.SourceCommitAt.Before(r.AfterCommitAt) && !r.SourceCommitAt.After(r.StartedAt)
}

func (r recoveryWitness) projection() map[string]any {
	verified := r.verified()
	proof := map[string]any{"targetAt": r.TargetAt, "startedAt": r.StartedAt, "finishedAt": r.FinishedAt,
		"rtoSeconds": r.FinishedAt.Sub(r.StartedAt).Seconds(), "confirmedBefore": r.ConfirmedBefore,
		"excludedAfter": r.ExcludedAfter, "verification": "reported"}
	if backupName.MatchString(r.ID) {
		proof["id"], proof["verification"] = r.ID, "incomplete"
	}
	if verified {
		proof["verification"], proof["isolated"] = "verified", true
		if backupName.MatchString(r.TargetID) {
			proof["targetId"] = r.TargetID
		}
		proof["beforeMarker"], proof["afterMarker"] = r.BeforeMarker, r.AfterMarker
		proof["restoredCommitAt"], proof["sourceCommitAt"], proof["afterCommitAt"] = r.RestoredCommitAt, r.SourceCommitAt, r.AfterCommitAt
	}
	if (verified || r.ID == "") && r.ConfirmedBefore && r.ExcludedAfter && !r.RestoredCommitAt.IsZero() &&
		!r.RestoredCommitAt.After(r.TargetAt) && !r.SourceCommitAt.Before(r.RestoredCommitAt) {
		proof["rpoSeconds"] = r.SourceCommitAt.Sub(r.RestoredCommitAt).Seconds()
	}
	return proof
}

func refName(value string) string {
	if backupName.MatchString(value) {
		return value
	}
	return ""
}

func validWAL(value string) string {
	if walName.MatchString(value) {
		return value
	}
	return ""
}

func checkedWAL(check backupWALCheck) (map[string]any, bool) {
	size := check.SegmentBytes
	if check.RepoKey <= 0 || check.DatabaseID <= 0 || size < 1<<20 || size > 1<<30 || size&(size-1) != 0 ||
		validWAL(check.From) == "" || validWAL(check.To) == "" || check.From[:8] != check.To[:8] || len(check.Segments) > 8192 {
		return nil, false
	}
	perLog := (1 << 32) / size
	start, validStart := walIndex(check.From, perLog)
	end, validEnd := walIndex(check.To, perLog)
	if !validStart || !validEnd || end < start || end-start >= 8192 {
		return nil, false
	}
	present := make(map[uint64]bool, len(check.Segments))
	for _, segment := range check.Segments {
		if validWAL(segment) != "" && segment[:8] == check.From[:8] {
			if index, valid := walIndex(segment, perLog); valid && index >= start && index <= end {
				present[index] = true
			}
		}
	}
	missing := []string{}
	for segment := start; segment <= end && len(missing) < 32; segment++ {
		if !present[segment] {
			missing = append(missing, fmt.Sprintf("%s%08X%08X", check.From[:8], segment/perLog, segment%perLog))
		}
	}
	return map[string]any{"repoKey": check.RepoKey, "databaseId": check.DatabaseID, "first": check.From, "last": check.To,
		"present": len(present), "missingCount": int(end-start+1) - len(present), "missing": missing}, true
}

func walIndex(name string, perLog uint64) (uint64, bool) {
	log, _ := strconv.ParseUint(name[8:16], 16, 32)
	segment, _ := strconv.ParseUint(name[16:], 16, 32)
	return log*perLog + segment, segment < perLog
}

// StoreBackup reads the operator's separate PostgreSQL 18 monitoring-store manifest.
func StoreBackup(now time.Time) map[string]any {
	path := os.Getenv("DBSPHERE_STORE_BACKUP_MANIFEST")
	if directory := os.Getenv("DBSPHERE_BACKUP_INFO_DIR"); path == "" && directory != "" {
		path = filepath.Join(directory, "monitor-store.json")
	}
	if path == "" {
		return map[string]any{"status": "not_configured", "reason": "尚未接入 PG18 备份清单"}
	}
	fallback := map[string]any{"status": "unavailable", "reason": "PG18 备份清单缺失、无权限或内容无效"}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fallback
	}
	file, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer file.Close()
	payload, err := boundedBackupFile(file, 64<<10)
	if err != nil {
		return fallback
	}
	var manifest struct {
		Status, BackupID string
		SHA256           string
		CompletedAt      time.Time
		RestorePointAt   time.Time
		RestoreTests     []storeRestore
	}
	if err := json.Unmarshal(payload, &manifest); err != nil || manifest.Status != "success" ||
		!backupName.MatchString(manifest.BackupID) || manifest.CompletedAt.IsZero() ||
		manifest.CompletedAt.After(now.Add(5*time.Minute)) || manifest.RestorePointAt.After(manifest.CompletedAt) {
		return fallback
	}
	result := map[string]any{"status": "ok", "backupId": manifest.BackupID, "completedAt": manifest.CompletedAt,
		"ageHours": math.Round(max(0, now.Sub(manifest.CompletedAt).Hours())*10) / 10}
	if !manifest.RestorePointAt.IsZero() {
		result["restorePointAt"] = manifest.RestorePointAt
	}
	tests := []map[string]any{}
	for _, restore := range manifest.RestoreTests[:min(len(manifest.RestoreTests), 20)] {
		if !archiveSHA256.MatchString(manifest.SHA256) || restore.BackupID != manifest.BackupID ||
			restore.ArchiveSHA256 != manifest.SHA256 || !restore.validTimes(now) ||
			restore.StartedAt.Before(manifest.CompletedAt) || !restore.TargetAt.Equal(manifest.RestorePointAt) {
			continue
		}
		proof := restore.projection()
		proof["archiveSha256"], proof["backupId"] = restore.ArchiveSHA256, restore.BackupID
		proof["eventsReadable"], proof["configurationReadable"], proof["samplesReadable"] = restore.EventsReadable, restore.ConfigurationReadable, restore.SamplesReadable
		if !restore.EventsReadable || !restore.ConfigurationReadable || !restore.SamplesReadable {
			proof["verification"] = "incomplete"
			delete(proof, "rpoSeconds")
		}
		tests = append(tests, proof)
	}
	result["restoreTests"] = tests
	return result
}

var archiveSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type storeRestore struct {
	BackupID              string `json:"backupId"`
	ArchiveSHA256         string `json:"archiveSha256"`
	EventsReadable        bool   `json:"eventsReadable"`
	ConfigurationReadable bool   `json:"configurationReadable"`
	SamplesReadable       bool   `json:"samplesReadable"`
	recoveryWitness
}
