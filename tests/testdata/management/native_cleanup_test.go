//go:build debug && cgo && (darwin || linux)

package management

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

// Use CPA's actual file watcher and persistent auth manager, not a mock of the
// host save callback, to verify that removed files stay removed after reload.
func TestMiniMaxNativeCredentialCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Error("unexpected upstream request")
		}
		io.WriteString(w, `{"data":[{"id":"MiniMax-M3"}]}`)
	}))
	defer upstream.Close()
	dir, authDir := t.TempDir(), t.TempDir()
	binary, err := os.ReadFile(os.Getenv("CPA_NATIVE_PLUGIN"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpa-minimax"+pluginhost.PluginExtension(runtime.GOOS)), binary, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: authDir, Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	apply := func(keys string) {
		t.Helper()
		var item config.PluginInstanceConfig
		settings := fmt.Sprintf("enabled: true\nbase-url: %s\nallow-http: true\napi-keys: %s\n", upstream.URL, keys)
		if err := yaml.Unmarshal([]byte(settings), &item); err != nil {
			t.Fatal(err)
		}
		next := *cfg
		next.Plugins.Configs = map[string]config.PluginInstanceConfig{"cpa-minimax": item}
		cfg = &next
		host.ApplyConfig(ctx, cfg)
		if len(host.RegisteredPlugins()) != 1 {
			t.Fatal("plugin failed to register")
		}
	}
	apply("[{name: A, value: offline-a}, {name: B, value: offline-b}]")
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, &coreauth.FillFirstSelector{}, nil)
	host.SetAuthManager(manager)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	watch, err := watcher.NewWatcher(configPath, authDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Stop()
	watch.SetConfig(cfg)
	watch.SetPluginAuthParser(host)
	updates := make(chan watcher.AuthUpdate, 32)
	watch.SetAuthUpdateQueue(updates)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-updates:
				if update.Action == watcher.AuthUpdateActionDelete {
					manager.Remove(ctx, update.ID)
				} else if _, exists := manager.GetByID(update.ID); exists {
					_, _ = manager.Update(ctx, update.Auth)
				} else {
					_, _ = manager.Register(ctx, update.Auth)
				}
			}
		}
	}()
	defer func() { cancel(); <-consumerDone }()
	if err := watch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor := func(check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for real host watcher")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitFor(func() bool { return len(manager.List()) == 2 })
	// CPA's live manager and root-directory watcher do not list this nested
	// file. Cleanup must discover it on disk, including its uppercase suffix.
	writeUnlisted := func(name string) string {
		t.Helper()
		nested := filepath.Join(authDir, "nested")
		if err := os.MkdirAll(nested, 0700); err != nil {
			t.Fatal(err)
		}
		key := "unlisted-" + name
		data := fmt.Sprintf(`{"type":"minimax","id":"minimax-key-%x","api_key":%q}`, sha256.Sum256([]byte(key)), key)
		path := filepath.Join(nested, name)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	unlisted := writeUnlisted("Removed.JSON")
	var removedID, removedPath, keptPath string
	for _, auth := range manager.List() {
		if auth.Attributes["api_key"] == "offline-a" {
			removedID, removedPath = auth.ID, auth.Attributes["path"]
		} else {
			keptPath = auth.Attributes["path"]
		}
	}
	if removedPath == "" || keptPath == "" {
		t.Fatal("watcher did not parse plugin credentials")
	}
	apply("[{name: B, value: offline-b}]")
	if _, err := os.Stat(unlisted); !os.IsNotExist(err) {
		t.Fatal("unlisted nested credential was not cleaned up")
	}
	if _, err := os.Stat(removedPath); !os.IsNotExist(err) {
		t.Fatal("removed key's file still exists")
	}
	if auth, exists := manager.GetByID(removedID); exists && !auth.Disabled {
		t.Fatal("removed key still enabled in runtime")
	}
	if _, err := os.Stat(keptPath); err != nil {
		t.Fatal("configured credential was removed")
	}
	unlisted = writeUnlisted("Idle.JsOn")
	apply("[]")
	if _, err := os.Stat(unlisted); !os.IsNotExist(err) {
		t.Fatal("keyless cleanup missed unlisted nested credential")
	}
	waitFor(func() bool {
		for _, auth := range manager.List() {
			if !auth.Disabled {
				return false
			}
		}
		return true
	})
	files, err := os.ReadDir(authDir)
	if err != nil || len(files) != 1 || files[0].Name() != "nested" {
		t.Fatal("keyless plugin retained credential files")
	}
	// Re-running config application must not resurrect either removed record.
	apply("[]")
	if files, _ := os.ReadDir(authDir); len(files) != 1 || files[0].Name() != "nested" {
		t.Fatal("reload recreated removed credentials")
	}
}
