package testutil

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

type FakeSeaweed struct {
	mu sync.Mutex

	files   map[string][]byte
	buckets map[string]map[string][]byte

	FailFilerWrites bool
	FailS3 bool
	Topology TopologyReport

	filerServer  *httptest.Server
	s3Server     *httptest.Server
	masterServer *httptest.Server
}

type TopologyReport struct {
	DataCenters   int
	Racks         int
	VolumeServers int
	Volumes       int
	Max           int
	Free          int
}

func NewFakeSeaweed() *FakeSeaweed {
	f := &FakeSeaweed{
		files:   map[string][]byte{},
		buckets: map[string]map[string][]byte{},
		Topology: TopologyReport{
			DataCenters: 1, Racks: 1, VolumeServers: 1, Volumes: 3, Max: 7, Free: 4,
		},
	}
	f.filerServer = httptest.NewServer(http.HandlerFunc(f.serveFiler))
	f.s3Server = httptest.NewServer(http.HandlerFunc(f.serveS3))
	f.masterServer = httptest.NewServer(http.HandlerFunc(f.serveMaster))
	return f
}

func (f *FakeSeaweed) Close() {
	f.filerServer.Close()
	f.s3Server.Close()
	f.masterServer.Close()
}

func (f *FakeSeaweed) FilerURL() string { return f.filerServer.URL }

func (f *FakeSeaweed) S3URL() string { return f.s3Server.URL }

func (f *FakeSeaweed) MasterURL() string { return f.masterServer.URL }

func (f *FakeSeaweed) File(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path]
	return b, ok
}

func (f *FakeSeaweed) SetFile(path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
}

func (f *FakeSeaweed) Buckets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.buckets))
	for name := range f.buckets {
		out = append(out, name)
	}
	return out
}

func (f *FakeSeaweed) HasBucket(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.buckets[name]
	return ok
}

func (f *FakeSeaweed) PutObject(bucket, key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets[bucket] == nil {
		f.buckets[bucket] = map[string][]byte{}
	}
	f.buckets[bucket][key] = data
}

func (f *FakeSeaweed) ObjectCount(bucket string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.buckets[bucket])
}


func (f *FakeSeaweed) serveFiler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		content, ok := f.files[path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)

	case http.MethodPost, http.MethodPut:
		f.mu.Lock()
		fail := f.FailFilerWrites
		f.mu.Unlock()
		if fail {
			http.Error(w, "filer unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Has("tagging") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = file.Close() }()
		content, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.files[path] = content
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		f.mu.Lock()
		delete(f.files, path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}


func (f *FakeSeaweed) serveMaster(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/cluster/healthz":
		w.WriteHeader(http.StatusOK)
	case "/cluster/status":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"IsLeader":true,"Leader":"master-0:9333","Peers":[]}`)
	case "/dir/status":
		f.mu.Lock()
		t := f.Topology
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		nodes := make([]string, 0, t.VolumeServers)
		for i := 0; i < t.VolumeServers; i++ {
			nodes = append(nodes, fmt.Sprintf(
				`{"Url":"volume-%d:8080","PublicUrl":"volume-%d:8080","Volumes":%d,"Max":%d,"Free":%d}`,
				i, i, t.Volumes, t.Max, t.Free))
		}
		racks := make([]string, 0, t.Racks)
		for i := 0; i < t.Racks; i++ {
			racks = append(racks, fmt.Sprintf(`{"Id":"rack%d","DataNodes":[%s]}`, i, strings.Join(nodes, ",")))
		}
		dcs := make([]string, 0, t.DataCenters)
		for i := 0; i < t.DataCenters; i++ {
			dcs = append(dcs, fmt.Sprintf(`{"Id":"dc%d","Racks":[%s]}`, i, strings.Join(racks, ",")))
		}
		fmt.Fprintf(w, `{"Topology":{"DataCenters":[%s],"Free":%d,"Max":%d},"Version":"fake"}`,
			strings.Join(dcs, ","), t.Free, t.Max)
	default:
		http.NotFound(w, r)
	}
}


type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Buckets struct {
		Bucket []bucketEntry `xml:"Bucket"`
	} `xml:"Buckets"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listBucketResult struct {
	XMLName     xml.Name      `xml:"ListBucketResult"`
	Name        string        `xml:"Name"`
	KeyCount    int           `xml:"KeyCount"`
	MaxKeys     int           `xml:"MaxKeys"`
	IsTruncated bool          `xml:"IsTruncated"`
	Contents    []objectEntry `xml:"Contents"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	Size         int    `xml:"Size"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
}

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

func (f *FakeSeaweed) serveS3(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	failing := f.FailS3
	f.mu.Unlock()
	if failing {
		http.Error(w, "s3 unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/status" {
		w.WriteHeader(http.StatusOK)
		return
	}

	trimmed := strings.Trim(r.URL.Path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	bucket := ""
	if parts[0] != "" {
		bucket = parts[0]
	}

	if bucket == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		out := listAllMyBucketsResult{}
		for name := range f.buckets {
			out.Buckets.Bucket = append(out.Buckets.Bucket, bucketEntry{
				Name:         name,
				CreationDate: time.Unix(0, 0).UTC().Format(time.RFC3339),
			})
		}
		f.mu.Unlock()
		writeXML(w, out)
		return
	}

	switch r.Method {
	case http.MethodHead:
		if !f.HasBucket(bucket) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodPut:
		f.mu.Lock()
		if _, ok := f.buckets[bucket]; !ok {
			f.buckets[bucket] = map[string][]byte{}
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		f.mu.Lock()
		objects := len(f.buckets[bucket])
		_, exists := f.buckets[bucket]
		f.mu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if objects > 0 {
			writeS3Error(w, http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty")
			return
		}
		f.mu.Lock()
		delete(f.buckets, bucket)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		f.mu.Lock()
		contents, ok := f.buckets[bucket]
		f.mu.Unlock()
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
			return
		}
		out := listBucketResult{Name: bucket, MaxKeys: 1000}
		for key, data := range contents {
			out.Contents = append(out.Contents, objectEntry{
				Key:          key,
				Size:         len(data),
				LastModified: time.Unix(0, 0).UTC().Format(time.RFC3339),
				ETag:         `"fake"`,
			})
		}
		out.KeyCount = len(out.Contents)
		writeXML(w, out)

	case http.MethodPost:
		if !r.URL.Query().Has("delete") {
			http.Error(w, "unsupported POST", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req deleteRequest
		if err := xml.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		for _, o := range req.Objects {
			delete(f.buckets[bucket], o.Key)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http:

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w,
		`<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`,
		code, message)
}
