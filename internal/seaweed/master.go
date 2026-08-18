package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type MasterClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewMasterClient(baseURL string) *MasterClient {
	return &MasterClient{
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

type dirStatusResponse struct {
	Topology struct {
		DataCenters []struct {
			ID    string `json:"Id"`
			Racks []struct {
				ID        string `json:"Id"`
				DataNodes []struct {
					URL       string `json:"Url"`
					PublicURL string `json:"PublicUrl"`
					Volumes   int32  `json:"Volumes"`
					Max       int32  `json:"Max"`
					Free      int32  `json:"Free"`
				} `json:"DataNodes"`
			} `json:"Racks"`
		} `json:"DataCenters"`
		Free int32 `json:"Free"`
		Max  int32 `json:"Max"`
	} `json:"Topology"`
	Version string `json:"Version"`
}

type Topology struct {
	DataCenters   int32
	Racks         int32
	VolumeServers int32
	ActiveVolumes int32
	MaxVolumes    int32
	FreeVolumes   int32
	Version       string
}

func (c *MasterClient) Topology(ctx context.Context) (*Topology, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/dir/status", nil)
	if err != nil {
		return nil, fmt.Errorf("build master status request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("master status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("master status: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed dirStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode master status: %w", err)
	}

	t := &Topology{
		Version:     parsed.Version,
		MaxVolumes:  parsed.Topology.Max,
		FreeVolumes: parsed.Topology.Free,
	}
	for _, dc := range parsed.Topology.DataCenters {
		t.DataCenters++
		for _, rack := range dc.Racks {
			t.Racks++
			for _, node := range rack.DataNodes {
				t.VolumeServers++
				t.ActiveVolumes += node.Volumes
			}
		}
	}
	return t, nil
}

type clusterStatusResponse struct {
	IsLeader bool     `json:"IsLeader"`
	Leader   string   `json:"Leader"`
	Peers    []string `json:"Peers"`
}

func (c *MasterClient) Leader(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/cluster/status", nil)
	if err != nil {
		return "", fmt.Errorf("build cluster status request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cluster status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("cluster status: %s", resp.Status)
	}
	var parsed clusterStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decode cluster status: %w", err)
	}
	return parsed.Leader, nil
}

func (c *MasterClient) Healthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/cluster/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("master healthz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("master healthz: status %s", resp.Status)
	}
	return nil
}
