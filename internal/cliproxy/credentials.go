package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

func (m *Manager) Credentials(ctx context.Context) ([]Credential, error) {
	if m.Running() {
		var result struct {
			Files []Credential `json:"files"`
		}
		err := m.Client().Do(ctx, "GET", "/credentials", nil, &result)
		if err == nil {
			snapshot := m.Runtime()
			for i := range result.Files {
				for j := range snapshot.Accounts {
					if snapshot.Accounts[j].ID == result.Files[i].ID {
						details := snapshot.Accounts[j]
						result.Files[i].Details = &details
						break
					}
				}
			}
		}
		return result.Files, err
	}
	files, err := m.ReadCredentials()
	if err != nil {
		return nil, err
	}
	out := make([]Credential, 0, len(files))
	for name, raw := range files {
		// Decode only fields allowed in the management view.
		var meta struct {
			Type     string `json:"type"`
			Email    string `json:"email"`
			Label    string `json:"label"`
			Disabled bool   `json:"disabled"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, err
		}
		id := name
		if runtime.GOOS == "windows" {
			id = strings.ToLower(id) // Match the SDK's file-backed auth IDs.
		}
		out = append(out, Credential{ID: id, Name: name, Provider: strings.ToLower(strings.TrimSpace(meta.Type)),
			Email: meta.Email, Label: meta.Label, Disabled: meta.Disabled, Unavailable: true, Status: "stopped"})
		var document map[string]any
		if json.Unmarshal(raw, &document) == nil {
			auth := &coreauth.Auth{ID: id, FileName: name, Provider: meta.Type, Metadata: document, Disabled: meta.Disabled}
			details := projectAccount(auth, nil, time.Now())
			details.Ready = false
			details.Reason = "stopped"
			out[len(out)-1].Details = &details
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadCredentials is used only for server-side transfer of a completed OAuth
// login into its owning pool. Secret-bearing documents never enter API views.
func (m *Manager) ReadCredentials() (map[string]json.RawMessage, error) {
	dir := filepath.Join(m.root, "auths")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 2<<20 {
			return nil, fmt.Errorf("凭据文件无效或过大")
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil || !json.Valid(raw) {
			return nil, fmt.Errorf("无法读取账号凭据")
		}
		out[entry.Name()] = raw
	}
	return out, nil
}

// SeedCredentials is only used before a new pool starts.
func (m *Manager) SeedCredentials(files map[string]json.RawMessage) error {
	dir := filepath.Join(m.root, "auths")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for name, raw := range files {
		if filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("凭据文件名无效")
		}
		if err := writePrivate(filepath.Join(dir, name), raw); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) CredentialCount() int {
	entries, _ := os.ReadDir(filepath.Join(m.root, "auths"))
	count := 0
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count
}

// DiscardLogin removes only private temporary login directories created by the
// gateway. Persistent pool directories are never removed by this operation.
func (m *Manager) DiscardLogin() {
	m.Close()
	root, err := filepath.Abs(m.root)
	if err == nil && strings.HasPrefix(filepath.Base(root), "login-") && filepath.Base(filepath.Dir(root)) == "pools" {
		_ = os.RemoveAll(root)
	}
}
