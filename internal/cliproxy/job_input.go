package cliproxy

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

var operationIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Secret-bearing import inputs live beside the pool's private auth directory,
// not in job API responses, gateway logs or the upstream SDK configuration.
func (m *Manager) SaveJobInput(id string, raw []byte) error {
	if !operationIDPattern.MatchString(id) || len(raw) > 24<<20 {
		return errors.New("导入任务无效")
	}
	dir := filepath.Join(m.root, "operation-inputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, id+".json"), raw)
}
func (m *Manager) ReadJobInput(id string) ([]byte, error) {
	if !operationIDPattern.MatchString(id) {
		return nil, errors.New("导入任务无效")
	}
	return os.ReadFile(filepath.Join(m.root, "operation-inputs", id+".json"))
}
func (m *Manager) RemoveJobInput(id string) error {
	if !operationIDPattern.MatchString(id) {
		return errors.New("导入任务无效")
	}
	err := os.Remove(filepath.Join(m.root, "operation-inputs", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
