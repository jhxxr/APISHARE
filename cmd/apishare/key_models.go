package main

import "strings"

// Key permissions use client-facing IDs, before any upstream model mapping.
func keyModelIDs(raw string) []string {
	seen := make(map[string]bool)
	ids := make([]string, 0)
	for _, id := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == '\r'
	}) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func normalizeKeyModels(raw string) string {
	return strings.Join(keyModelIDs(raw), ",")
}

func (ak ApiKey) allowsModel(model string) bool {
	// Unbound keys cannot call any model, including keys created before migration.
	for _, id := range keyModelIDs(ak.Models) {
		if id == model {
			return true
		}
	}
	return false
}

func (ak ApiKey) filterModels(models []catalogModel) []catalogModel {
	allowed := make(map[string]bool)
	for _, id := range keyModelIDs(ak.Models) {
		allowed[id] = true
	}
	filtered := make([]catalogModel, 0)
	for _, model := range models {
		if allowed[model.ID] {
			filtered = append(filtered, model)
		}
	}
	return filtered
}
