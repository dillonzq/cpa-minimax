package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/config"
)

// CPA rolls back a failed hot reload by registering the quiesced plugin again.
func TestRegisterAfterQuiesceResumesWork(t *testing.T) {
	m, _ := newExecManager(t)
	if raw, _ := m.HandleCall(pluginabi.MethodPluginQuiesce, nil); !decodeEnv(t, raw).OK {
		t.Fatal("quiesce failed")
	}
	if raw, _ := m.HandleCall(pluginabi.MethodModelStatic, nil); decodeEnv(t, raw).OK {
		t.Fatal("quiesced plugin admitted work")
	}
	if raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)); !decodeEnv(t, raw).OK {
		t.Fatalf("rollback register rejected: %+v", decodeEnv(t, raw).Error)
	}
	if raw, _ := m.HandleCall(pluginabi.MethodModelStatic, nil); !decodeEnv(t, raw).OK {
		t.Fatal("work not re-admitted after rollback register")
	}
}

func TestMaterializeSkipsUnreadableRecordsAndTrimsKeys(t *testing.T) {
	dir := t.TempDir()
	padded := filepath.Join(dir, "Padded.json")
	if err := os.WriteFile(padded, []byte(`{"type":"minimax","api_key":" sk-cp-padded \n"}`), 0600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "Broken.json")
	if err := os.WriteFile(broken, []byte(`{not json`), 0600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{Name: "Padded.json", Provider: ProviderID, Path: padded},
				{Name: "Broken.json", Provider: ProviderID, Path: broken},
				{Name: "Relative.json", Provider: ProviderID, Path: "Relative.json"},
			}}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	cfg := config.Config{APIKeys: []config.APIKey{{Value: "sk-cp-padded", Type: config.KeyTypeSubscription}}}
	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("unreadable records must not fail registration: %v", err)
	}
	if n := len(f.callsOf(pluginabi.MethodHostAuthSave)); n != 0 {
		t.Fatalf("whitespace-padded record duplicated: %d saves", n)
	}
}

func TestRefreshOnceWithoutBridgeDoesNotPanic(t *testing.T) {
	m := NewManager(nil)
	yaml := "catalog:\n  discover: false\nprotocols:\n  messages: false\n  chat-completions: false\n  responses: false\nmodels:\n  - id: MiniMax-M3\n" + lifecycleYAML
	raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, lifecycleRequestBody(yaml))
	if env := decodeEnv(t, raw); env.Error != nil && env.Error.Code == "plugin_error" {
		t.Fatalf("register panicked without a bridge: %+v", env.Error)
	}
}
