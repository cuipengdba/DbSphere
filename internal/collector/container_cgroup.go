package collector

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var blockDevice = regexp.MustCompile(`^[0-9]{1,10}:[0-9]{1,10}$`)

func RuntimeContainerID(value string) string {
	if _, id, ok := strings.Cut(value, "://"); ok {
		value = id
	}
	if containerID.MatchString(value) {
		return value
	}
	return ""
}

type ContainerPoll struct {
	Status     string                       `json:"status"`
	Source     string                       `json:"source"`
	ID         string                       `json:"id"`
	ObservedAt time.Time                    `json:"observedAt"`
	Generation string                       `json:"generation,omitempty"`
	Memory     map[string]uint64            `json:"memory,omitempty"`
	Devices    map[string]map[string]uint64 `json:"devices,omitempty"`
	Limits     map[string]map[string]uint64 `json:"limits,omitempty"`
	Pressure   map[string]float64           `json:"pressure,omitempty"`
	States     map[string]string            `json:"states,omitempty"`
}

type cgroupPaths struct{ unified, memory, blkio string }

type cgroupReader struct {
	files fs.FS
	mu    sync.Mutex
	index map[string]cgroupPaths
	at    time.Time
}

// ContainerCgroupHandler observes only known runtime directories in a read-only
// cgroup mount. The client provides a full runtime ID, never a filesystem path.
func ContainerCgroupHandler(files fs.FS, token string) (http.Handler, error) {
	if len(token) < 24 || strings.ContainsAny(token, " \r\n\t") {
		return nil, errors.New("container agent requires a bearer token of at least 24 characters")
	}
	reader := &cgroupReader{files: files}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/container/{id}", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id := r.PathValue("id")
		if !containerID.MatchString(id) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(reader.poll(r.Context(), id))
	})
	return mux, nil
}

func (r *cgroupReader) locate(ctx context.Context, id string) cgroupPaths {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.at) < 5*time.Second {
		return r.index[id]
	}
	index := make(map[string]cgroupPaths)
	visited := 0
	_ = fs.WalkDir(r.files, ".", func(name string, dir fs.DirEntry, err error) error {
		visited++
		if visited > 125000 || ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil || !dir.IsDir() {
			return nil
		}
		base := strings.TrimSuffix(path.Base(name), ".scope")
		for _, prefix := range []string{"docker-", "cri-containerd-", "crio-", "libpod-"} {
			base = strings.TrimPrefix(base, prefix)
		}
		if !containerID.MatchString(base) {
			return nil
		}
		first, _, _ := strings.Cut(name, "/")
		paths := index[base]
		switch first {
		case "memory":
			paths.memory = name
		case "blkio":
			paths.blkio = name
		default:
			// v1 controllers repeat the same runtime ID in several hierarchies.
			if _, err := fs.Stat(r.files, path.Join(name, "cgroup.procs")); err == nil {
				if _, err := fs.Stat(r.files, path.Join(name, "cgroup.type")); err == nil {
					paths.unified = name
				}
			}
		}
		index[base] = paths
		return fs.SkipDir
	})
	r.index, r.at = index, time.Now()
	return index[id]
}

func (r *cgroupReader) poll(ctx context.Context, id string) ContainerPoll {
	paths := r.locate(ctx, id)
	p := ContainerPoll{ID: id, Status: "not_found", ObservedAt: time.Now().UTC(), Source: "cgroup",
		Memory: map[string]uint64{}, Devices: map[string]map[string]uint64{}, Pressure: map[string]float64{}, States: map[string]string{}}
	if paths == (cgroupPaths{}) {
		return p
	}
	read := func(dir, name string) string {
		if dir == "" {
			p.States[name] = "unsupported"
			return ""
		}
		data, err := fs.ReadFile(r.files, path.Join(dir, name))
		switch {
		case errors.Is(err, fs.ErrPermission):
			p.States[name] = "denied"
		case errors.Is(err, fs.ErrNotExist):
			p.States[name] = "unsupported"
		case err != nil || len(data) > 196608:
			p.States[name] = "unavailable"
		default:
			p.States[name] = "ok"
			return string(data)
		}
		return ""
	}
	if paths.unified != "" {
		p.Source = "cgroup_v2"
		p.Generation = cgroupGeneration(r.files, paths.unified)
		p.Memory = cgroupMemory(read(paths.unified, "memory.stat"), false)
		if events := cgroupNumbers(read(paths.unified, "memory.events")); events != nil {
			for _, key := range []string{"high", "max", "oom", "oom_kill"} {
				if value, ok := events[key]; ok {
					p.Memory["events_"+key] = value
				}
			}
		}
		p.Devices = cgroupDeviceKeys(read(paths.unified, "io.stat"), []string{"rbytes", "wbytes", "rios", "wios"})
		p.Limits = cgroupDeviceKeys(read(paths.unified, "io.max"), []string{"rbps", "wbps", "riops", "wiops"})
		readPSI(p.Pressure, "io", read(paths.unified, "io.pressure"))
		readPSI(p.Pressure, "memory", read(paths.unified, "memory.pressure"))
	} else {
		p.Source = "cgroup_v1"
		p.Generation = cgroupGeneration(r.files, paths.memory) + ":" + cgroupGeneration(r.files, paths.blkio)
		p.Memory = cgroupMemory(read(paths.memory, "memory.stat"), true)
		readV1Devices(p.Devices, read(paths.blkio, "blkio.throttle.io_service_bytes_recursive"), "rbytes", "wbytes")
		readV1Devices(p.Devices, read(paths.blkio, "blkio.throttle.io_serviced_recursive"), "rios", "wios")
		p.States["io.pressure"], p.States["memory.pressure"] = "unsupported", "unsupported"
	}
	p.Status = "unsupported"
	for _, status := range p.States {
		if status == "denied" {
			p.Status = "denied"
		}
	}
	if len(p.Memory)+len(p.Devices)+len(p.Pressure) > 0 {
		p.Status = "ok"
	}
	return p
}

func cgroupNumbers(raw string) map[string]uint64 {
	result := map[string]uint64{}
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				result[fields[0]] = value
			}
		}
	}
	return result
}

func cgroupMemory(raw string, v1 bool) map[string]uint64 {
	values := cgroupNumbers(raw)
	result := map[string]uint64{}
	for _, key := range []string{"active_file", "inactive_file", "file", "file_dirty", "file_writeback", "workingset_refault_file", "pgscan", "pgsteal"} {
		name := key
		if v1 {
			switch key {
			case "file":
				name = "cache"
			case "file_dirty":
				name = "dirty"
			case "file_writeback":
				name = "writeback"
			}
			if value, ok := values["total_"+name]; ok {
				result[key] = value
				continue
			}
		}
		if value, ok := values[name]; ok {
			result[key] = value
		}
	}
	return result
}

func cgroupDeviceKeys(raw string, keys []string) map[string]map[string]uint64 {
	result := map[string]map[string]uint64{}
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !blockDevice.MatchString(fields[0]) || len(result) == 64 {
			continue
		}
		values := map[string]uint64{}
		for _, field := range fields[1:] {
			key, rawValue, _ := strings.Cut(field, "=")
			for _, allowed := range keys {
				if key == allowed {
					if value, err := strconv.ParseUint(rawValue, 10, 64); err == nil {
						values[key] = value
					}
				}
			}
		}
		if len(values) > 0 {
			result[fields[0]] = values
		}
	}
	return result
}

func readV1Devices(result map[string]map[string]uint64, raw, read, write string) {
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !blockDevice.MatchString(fields[0]) {
			continue
		}
		key := map[string]string{"Read": read, "Write": write}[fields[1]]
		value, err := strconv.ParseUint(fields[2], 10, 64)
		if key == "" || err != nil {
			continue
		}
		if result[fields[0]] == nil && len(result) < 64 {
			result[fields[0]] = map[string]uint64{}
		}
		if counters := result[fields[0]]; counters != nil {
			counters[key] = value
		}
	}
}

func readPSI(result map[string]float64, resource, raw string) {
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[0] != "some" && fields[0] != "full") {
			continue
		}
		for _, field := range fields[1:] {
			if value, found := strings.CutPrefix(field, "avg10="); found {
				if percent, err := strconv.ParseFloat(value, 64); err == nil && percent >= 0 && percent <= 100 {
					result[resource+"_"+fields[0]+"_avg10"] = percent
				}
			}
		}
	}
}
