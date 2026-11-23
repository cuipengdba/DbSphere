package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestContainerCgroupReportsIndependentGaugesAndDeviceCounters(t *testing.T) {
	id := strings.Repeat("a", 64)
	root := "system.slice/docker-" + id + ".scope/"
	files := fstest.MapFS{
		root + "cgroup.procs": {Data: []byte("101\n")}, root + "cgroup.type": {Data: []byte("domain\n")},
		root + "memory.stat": {Data: []byte("file 1600\nactive_file 800\ninactive_file 620\nworkingset_refault_file 5\n")},
		root + "io.stat":     {Data: []byte("8:16 rbytes=1048576 wbytes=2097152 rios=30 wios=40\n")},
		root + "io.max":      {Data: []byte("8:16 rbps=4000000 wbps=max riops=max wiops=max\n")},
		root + "io.pressure": {Data: []byte("some avg10=1.22 avg60=0.75 avg300=0.20 total=100\n")},
	}
	token := strings.Repeat("T", 32)
	handler, err := ContainerCgroupHandler(files, token)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/container/"+id, nil)
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, request)
	if denied.Code != http.StatusUnauthorized {
		t.Fatal("cgroup snapshot was returned without a token")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var p ContainerPoll
	if err := json.Unmarshal(response.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "ok" || p.Source != "cgroup_v2" || p.Memory["file"] != 1600 || p.Memory["active_file"] != 800 ||
		p.Memory["inactive_file"] != 620 || p.Devices["8:16"]["wios"] != 40 || p.Limits["8:16"]["rbps"] != 4000000 || p.Pressure["io_some_avg10"] != 1.22 {
		t.Fatalf("incorrect cgroup observation: %#v", p)
	}
	if _, exists := p.Limits["8:16"]["wbps"]; exists {
		t.Fatal("unlimited writes became a zero write limit")
	}
}

func TestContainerCgroupV1PreservesMissingPressure(t *testing.T) {
	id := strings.Repeat("1", 64)
	child := "/docker/" + id + "/"
	reader := cgroupReader{files: fstest.MapFS{
		"memory" + child + "memory.stat":                              {Data: []byte("active_file 10\ntotal_active_file 100\ntotal_inactive_file 80\ntotal_cache 210\n")},
		"blkio" + child + "blkio.throttle.io_service_bytes_recursive": {Data: []byte("8:0 Read 400\n8:0 Write 800\n8:0 Total 1200\nTotal 1200\n")},
	}}
	p := reader.poll(t.Context(), id)
	if p.Status != "ok" || p.Source != "cgroup_v1" || p.Memory["active_file"] != 100 || p.Memory["inactive_file"] != 80 ||
		p.Devices["8:0"]["rbytes"] != 400 || len(p.Pressure) > 0 || p.States["io.pressure"] != "unsupported" {
		t.Fatalf("incorrect cgroup v1 result: %#v", p)
	}
}
