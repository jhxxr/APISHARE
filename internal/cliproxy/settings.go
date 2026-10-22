package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

type PoolRouting struct {
	Strategy        string `json:"strategy"`
	SessionAffinity bool   `json:"session_affinity"`
}

func (m *Manager) Routing() (PoolRouting, error) {
	if err := m.ensureConfig(); err != nil {
		return PoolRouting{}, err
	}
	cfg, err := config.LoadConfig(filepath.Join(m.root, "config.yaml"))
	if err != nil {
		return PoolRouting{}, err
	}
	return PoolRouting{Strategy: cfg.Routing.Strategy, SessionAffinity: cfg.Routing.SessionAffinity}, nil
}

func (m *Manager) SetRouting(ctx context.Context, routing PoolRouting) error {
	if routing.Strategy != "round-robin" && routing.Strategy != "weighted-round-robin" && routing.Strategy != "fill-first" {
		return errors.New("不支持的池内调度策略")
	}
	m.op.Lock()
	defer m.op.Unlock()
	if err := m.ensureConfig(); err != nil {
		return err
	}
	if m.Running() {
		previous, err := m.Routing()
		if err != nil {
			return err
		}
		if err := m.Client().Do(ctx, "PUT", "/config/routing/strategy", routing.Strategy, nil); err != nil {
			return err
		}
		if err := m.Client().Do(ctx, "PUT", "/config/routing/session-affinity", routing.SessionAffinity, nil); err != nil {
			_ = m.Client().Do(ctx, "PUT", "/config/routing/strategy", previous.Strategy, nil)
			return err
		}
	} else {
		path := filepath.Join(m.root, "config.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var document yaml.Node
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return err
		}
		if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
			return errors.New("账号池配置无效")
		}
		root := document.Content[0]
		var node *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "routing" {
				node = root.Content[i+1]
			}
		}
		if node == nil {
			node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "routing"}, node)
		}
		if node.Kind != yaml.MappingNode {
			return errors.New("账号池调度配置无效")
		}
		for key, value := range map[string]any{"strategy": routing.Strategy, "session-affinity": routing.SessionAffinity} {
			var replacement yaml.Node
			if err := replacement.Encode(value); err != nil {
				return err
			}
			found := false
			for i := 0; i < len(node.Content); i += 2 {
				if node.Content[i].Value == key {
					replacement.HeadComment = node.Content[i+1].HeadComment
					replacement.LineComment = node.Content[i+1].LineComment
					node.Content[i+1] = &replacement
					found = true
				}
			}
			if !found {
				node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &replacement)
			}
		}
		raw, err = yaml.Marshal(&document)
		if err != nil {
			return err
		}
		if err := writePrivate(path, raw); err != nil {
			return err
		}
	}
	m.Touch()
	return nil
}

func (m *Manager) PatchCredential(ctx context.Context, name string, fields map[string]any) error {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
		return errors.New("凭据文件名无效")
	}
	if m.Running() {
		body := make(map[string]any, len(fields)+1)
		for key, value := range fields {
			body[key] = value
		}
		body["name"] = name
		return m.Client().Do(ctx, "PATCH", "/credentials/fields", body, nil)
	}
	files, err := m.ReadCredentials()
	if err != nil {
		return err
	}
	raw, ok := files[name]
	if !ok {
		return errors.New("账号不属于当前池")
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	for key, value := range fields {
		document[key] = value
	}
	raw, err = json.Marshal(document)
	if err != nil {
		return err
	}
	return m.SeedCredentials(map[string]json.RawMessage{name: raw})
}
