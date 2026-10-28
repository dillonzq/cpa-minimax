package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/config"
)

func TestAvailableAuthNameCaseInsensitiveCollisions(t *testing.T) {
	cases := []struct {
		name, label string
		existing    map[string]struct{}
		want        string
	}{
		{"existing filename", "Personal", map[string]struct{}{"personal.json": {}}, "Personal-2.json"},
		{"existing suffixes", "Personal", map[string]struct{}{"personal.json": {}, "PERSONAL-2.JSON": {}, "Personal-3.json": {}}, "Personal-4.json"},
		{"unicode case", "Équipe", map[string]struct{}{"équipe.json": {}}, "Équipe-2.json"},
		{"unicode fold", "Σ", map[string]struct{}{"ς.json": {}}, "Σ-2.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := availableAuthName(tc.label, tc.existing); got != tc.want {
				t.Fatalf("filename=%q, want %q", got, tc.want)
			}
		})
	}
	existing := map[string]struct{}{}
	for i, label := range []string{"Personal", "personal", "PERSONAL"} {
		got := availableAuthName(label, existing)
		want := []string{"Personal.json", "personal-2.json", "PERSONAL-3.json"}[i]
		if got != want {
			t.Fatalf("new credential filename=%q, want %q", got, want)
		}
		existing[got] = struct{}{}
	}
}

func TestAvailableAuthNameWindowsDevices(t *testing.T) {
	names := []string{"CON", "PRN", "AUX", "NUL"}
	for _, prefix := range []string{"COM", "LPT"} {
		for _, digit := range "123456789" {
			names = append(names, prefix+string(digit))
		}
	}
	for _, name := range names {
		for _, label := range []string{name, strings.ToLower(name)} {
			t.Run(label, func(t *testing.T) {
				want := "MiniMax-" + label + ".json"
				if got := availableAuthName(label, nil); got != want {
					t.Fatalf("filename=%q, want %q", got, want)
				}
				if got := availableAuthName(label, map[string]struct{}{strings.ToUpper(want): {}}); got != "MiniMax-"+label+"-2.json" {
					t.Fatalf("escaped filename collision: %q", got)
				}
			})
		}
	}
	// The existing sanitizer removes superscript digits and extension dots;
	// its output must remain usable without treating ordinary prefixes as devices.
	for _, label := range []string{"COM0", "COM10", "LPT0", "LPT10", "Console", "NUL-backup"} {
		if got := availableAuthName(label, nil); got != label+".json" {
			t.Fatalf("ordinary filename changed: %q", got)
		}
	}
	for _, label := range []string{"CON.", " NUL ", "COM¹", "LPT²"} {
		got := availableAuthName(label, nil)
		if got == "CON.json" || got == "NUL.json" || got == "COM¹.json" || got == "LPT².json" {
			t.Fatalf("sanitized filename is reserved: %q", got)
		}
	}
}

// Model the host's os.WriteFile behavior on a case-insensitive filesystem:
// a collision overwrites the original file. Registration must choose a new
// name, preserve existing bytes, and remain idempotent on the next call.
func TestMaterializeAuthNamesPreserveExistingFiles(t *testing.T) {
	dir := t.TempDir()
	original := []byte(`{"type":"minimax","id":"existing-id","api_key":"offline-existing","label":"Work","disabled":true}`)
	path := filepath.Join(dir, "personal.json")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			files := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
			for _, entry := range entries {
				var record struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				}
				raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(raw, &record); err != nil {
					return nil, err
				}
				files = append(files, pluginapi.HostAuthFileEntry{Name: entry.Name(), ID: record.ID, Provider: record.Type, Path: filepath.Join(dir, entry.Name())})
			}
			return hostOK(hostAuthListResponse{Files: files}), nil
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			target := filepath.Join(dir, req.Name)
			for _, entry := range entries {
				if strings.EqualFold(entry.Name(), req.Name) {
					target = filepath.Join(dir, entry.Name())
					break
				}
			}
			if err := os.WriteFile(target, req.JSON, 0600); err != nil {
				return nil, err
			}
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	cfg := config.Config{APIKeys: []config.APIKey{{Value: "offline-new-first", Name: "Personal"}, {Value: "offline-new-second", Name: "personal"}, {Value: "offline-new-third", Name: "CON"}}}
	for i := 0; i < 2; i++ {
		if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(original) {
		t.Fatal("registration overwrote existing credential")
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 3 {
		t.Fatalf("auth saves=%d, want 3", len(saves))
	}
	for i, want := range []string{"Personal-2.json", "personal-3.json", "MiniMax-CON.json"} {
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(saves[i].payload, &req); err != nil {
			t.Fatal(err)
		}
		if req.Name != want {
			t.Fatalf("saved name=%q, want %q", req.Name, want)
		}
		var record struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal(req.JSON, &record); err != nil {
			t.Fatal(err)
		}
		if record.Label != cfg.APIKeys[i].Name {
			t.Fatal("filename escaping changed the account label")
		}
	}
	if !m.bridge.WaitForInFlight(time.Second) {
		t.Fatal("auth callbacks did not drain")
	}
}
