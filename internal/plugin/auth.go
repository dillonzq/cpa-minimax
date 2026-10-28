package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/config"
)

// authRecord is the persisted credential shape. region and key_type make a
// file self-describing, so credentials added through the management API work
// without a matching api-keys entry.
type authRecord struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Label   string `json:"label"`
	APIKey  string `json:"api_key"`
	Region  string `json:"region,omitempty"`
	KeyType string `json:"key_type,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

type authProvider struct{ cfg config.Config }

var _ pluginapi.AuthProvider = authProvider{}

func (authProvider) Identifier() string { return ProviderID }

func (p authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	debugTrace("auth parse request provider=%s file=%s raw_bytes=%d", req.Provider, req.FileName, len(req.RawJSON))
	var raw struct {
		authRecord
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("minimax auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (raw.Type != ProviderID && raw.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	key := strings.TrimSpace(raw.APIKey)
	if key == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("minimax auth record has no api key")
	}
	if raw.ID == "" {
		raw.ID = req.FileName
	}
	entry, err := resolveCredential(p.cfg, key, raw.Region, raw.KeyType, raw.BaseURL)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	debugTrace("auth parse handled provider=%s file=%s id=%s region=%s key_type=%s api_key_length=%d", req.Provider, req.FileName, raw.ID, entry.Region, entry.Type, len(key))
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider: ProviderID, ID: raw.ID, FileName: req.FileName, Label: accountLabel(p.cfg, key, raw.Label, entry), StorageJSON: req.RawJSON,
		// A file-level priority set through CPA still wins over this default.
		Attributes: map[string]string{"api_key": key, "region": entry.Region, "key_type": entry.Type, "priority": strconv.Itoa(entry.Priority), "base_url": entry.BaseURL},
	}}, nil
}

// resolveCredential merges the configured entry for a key over the record's
// own fields: configuration wins, then the record, then defaults.
func resolveCredential(cfg config.Config, key, region, keyType, baseURL string) (config.APIKey, error) {
	if entry, ok := cfg.Key(key); ok {
		return entry, nil
	}
	entry := config.APIKey{Value: key, Region: cfg.Region}
	if entry.Region == "" {
		entry.Region = config.DefaultRegion
	}
	if strings.TrimSpace(region) != "" {
		r, ok := config.NormalizeRegion(region)
		if !ok {
			return entry, fmt.Errorf("minimax auth record has an unsupported region")
		}
		entry.Region = r
	}
	t, ok := config.ResolveKeyType(key, keyType)
	if !ok {
		return entry, fmt.Errorf("minimax auth record has an unsupported key_type")
	}
	if baseURL != "" {
		var err error
		entry.BaseURL, err = config.NormalizeCredentialBaseURL(baseURL, cfg.AllowHTTP)
		if err != nil {
			return entry, err
		}
	}
	entry.Type = t
	entry.Priority = config.DefaultPriority[t]
	return entry, nil
}

func (authProvider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("minimax login is unsupported; configure a manual api key")
}

func (authProvider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("minimax login is unsupported; configure a manual api key")
}

func (authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	debugTrace("auth refresh request provider=%s id=%s storage_json_bytes=%d attr_names=%v metadata_names=%v", req.AuthProvider, req.AuthID, len(req.StorageJSON), slices.Collect(maps.Keys(req.Attributes)), slices.Collect(maps.Keys(req.Metadata)))
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON, Metadata: req.Metadata, Attributes: req.Attributes}}, nil
}

// accountLabel is presentation only. A configured name wins, then an existing
// label, then a generated "MiniMax <plan> [CN] [n]" label numbered within
// keys of the same kind in configuration order.
func accountLabel(cfg config.Config, key, existing string, entry config.APIKey) string {
	if configured, ok := cfg.Key(key); ok {
		if name := strings.TrimSpace(configured.Name); name != "" {
			return name
		}
	}
	if label := strings.TrimSpace(existing); label != "" {
		return label
	}
	base := defaultLabel(entry)
	count, position := 0, 0
	for _, k := range cfg.APIKeys {
		if strings.TrimSpace(k.Name) != "" || defaultLabel(k) != base {
			continue
		}
		count++
		if k.Value == key {
			position = count
		}
	}
	if count > 1 && position > 0 {
		return fmt.Sprintf("%s %d", base, position)
	}
	return base
}

func defaultLabel(entry config.APIKey) string {
	label := "MiniMax API"
	if entry.Type == config.KeyTypeSubscription {
		label = "MiniMax M Plan"
	}
	if entry.Region == config.RegionCN {
		label += " CN"
	}
	return label
}
