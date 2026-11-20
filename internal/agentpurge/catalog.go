package agentpurge

import (
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

//go:embed neutral.md personas/*.md catalog.json
var bundled embed.FS

type Source struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	License    string `json:"license"`
	URL        string `json:"url"`
}

type Persona struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Order       int    `json:"order"`
	Source      Source `json:"source"`
	Text        string `json:"text"`
	Characters  int    `json:"characters"`
	SHA256      string `json:"sha256"`
}

var catalog, byID = loadCatalog()

func loadCatalog() ([]Persona, map[string]Persona) {
	var manifest struct {
		Personas []struct {
			Persona
			Asset      string `json:"asset"`
			RawSHA256  string `json:"raw_sha256"`
			BodySHA256 string `json:"body_sha256"`
		} `json:"personas"`
	}
	data, err := bundled.ReadFile("catalog.json")
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		panic("invalid bundled agent-purge catalog")
	}
	items := make([]Persona, 0, len(manifest.Personas))
	index := make(map[string]Persona, len(manifest.Personas))
	for _, row := range manifest.Personas {
		raw, err := bundled.ReadFile(row.Asset)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != row.RawSHA256 {
			panic("bundled agent-purge template differs from its source manifest: " + row.ID)
		}
		text := strings.TrimPrefix(string(raw), "\ufeff")
		if strings.HasPrefix(text, "---\n") || strings.HasPrefix(text, "---\r\n") {
			if end := strings.Index(text[3:], "\n---"); end >= 0 {
				text = text[end+7:]
			}
		}
		text = strings.TrimSpace(text)
		if row.ID == "" || text == "" || fmt.Sprintf("%x", sha256.Sum256([]byte(text))) != row.BodySHA256 {
			panic("invalid bundled agent-purge template body: " + row.ID)
		}
		if _, exists := index[row.ID]; exists {
			panic("duplicate bundled agent-purge template: " + row.ID)
		}
		item := row.Persona
		item.Source.URL = "https://github.com/" + item.Source.Repository + "/blob/" + item.Source.Revision + "/" + item.Source.Path
		item.Text, item.Characters, item.SHA256 = text, utf8.RuneCountInString(text), row.BodySHA256
		items = append(items, item)
		index[item.ID] = item
	}
	if _, exists := index[PersonaID]; !exists {
		panic("bundled agent-purge catalog is missing its default template")
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Order != items[j].Order {
			return items[i].Order < items[j].Order
		}
		return items[i].ID < items[j].ID
	})
	return items, index
}

func NormalizePersonaID(id string) string {
	if id = strings.TrimSpace(id); id == "" {
		return PersonaID
	}
	return id
}

// Lookup accepts only a compiled template ID; it never reads caller paths.
func Lookup(id string) (Persona, bool) {
	item, ok := byID[NormalizePersonaID(id)]
	return item, ok
}

// Personas returns a copy so API callers cannot mutate the compiled catalog.
func Personas() []Persona { return append([]Persona(nil), catalog...) }
