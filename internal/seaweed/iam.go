package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type IAMConfig struct {
	Identities []Identity `json:"identities"`
}

type Identity struct {
	Name        string          `json:"name"`
	Credentials []Credential    `json:"credentials,omitempty"`
	Actions     []string        `json:"actions"`
	Account     json.RawMessage `json:"account,omitempty"`
}

type Credential struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

var AdminActions = []string{"Admin", "Read", "Write", "List", "Tagging"}

func ParseIAMConfig(data []byte) (*IAMConfig, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return &IAMConfig{}, nil
	}
	var cfg IAMConfig
	if err := json.Unmarshal([]byte(trimmed), &cfg); err != nil {
		return nil, fmt.Errorf("parse identity.json: %w", err)
	}
	return &cfg, nil
}

func (c *IAMConfig) Marshal() ([]byte, error) {
	sorted := append([]Identity(nil), c.Identities...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i := range sorted {
		sorted[i].Actions = normalizeActions(sorted[i].Actions)
	}
	out := IAMConfig{Identities: sorted}
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal identity.json: %w", err)
	}
	return append(data, '\n'), nil
}

func normalizeActions(actions []string) []string {
	seen := make(map[string]struct{}, len(actions))
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func (c *IAMConfig) Get(name string) (*Identity, bool) {
	for i := range c.Identities {
		if c.Identities[i].Name == name {
			return &c.Identities[i], true
		}
	}
	return nil, false
}

func (c *IAMConfig) Upsert(id Identity) bool {
	id.Actions = normalizeActions(id.Actions)
	for i := range c.Identities {
		if c.Identities[i].Name != id.Name {
			continue
		}
		existing := c.Identities[i]
		existing.Actions = normalizeActions(existing.Actions)
		if identitiesEqual(existing, id) {
			return false
		}
		c.Identities[i] = id
		return true
	}
	c.Identities = append(c.Identities, id)
	return true
}

func (c *IAMConfig) Remove(name string) bool {
	for i := range c.Identities {
		if c.Identities[i].Name == name {
			c.Identities = append(c.Identities[:i], c.Identities[i+1:]...)
			return true
		}
	}
	return false
}

func identitiesEqual(a, b Identity) bool {
	if a.Name != b.Name || len(a.Actions) != len(b.Actions) || len(a.Credentials) != len(b.Credentials) {
		return false
	}
	for i := range a.Actions {
		if a.Actions[i] != b.Actions[i] {
			return false
		}
	}
	for i := range a.Credentials {
		if a.Credentials[i] != b.Credentials[i] {
			return false
		}
	}
	return true
}

func BuildAction(verb, bucket, prefix string) string {
	if bucket == "" || bucket == "*" {
		return verb
	}
	if prefix == "" {
		return verb + ":" + bucket
	}
	return verb + ":" + bucket + "/" + strings.TrimPrefix(prefix, "/")
}

type IAMStore struct {
	Filer *FilerClient
	Path  string
}

func NewIAMStore(filer *FilerClient, path string) *IAMStore {
	return &IAMStore{Filer: filer, Path: path}
}

func (s *IAMStore) Load(ctx context.Context) (*IAMConfig, error) {
	data, err := s.Filer.ReadFile(ctx, s.Path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return &IAMConfig{}, nil
		}
		return nil, err
	}
	return ParseIAMConfig(data)
}

func (s *IAMStore) Save(ctx context.Context, cfg *IAMConfig) error {
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	return s.Filer.WriteFile(ctx, s.Path, data)
}

func (s *IAMStore) Mutate(ctx context.Context, fn func(*IAMConfig) (bool, error)) (bool, error) {
	cfg, err := s.Load(ctx)
	if err != nil {
		return false, err
	}
	changed, err := fn(cfg)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := s.Save(ctx, cfg); err != nil {
		return false, err
	}
	return true, nil
}
