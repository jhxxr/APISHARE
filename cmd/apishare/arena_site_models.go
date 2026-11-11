package main

import (
	"net/url"
	"sort"
	"strings"
	"unicode"
)

// Public membership is model-level discovery, NOT permission to attribute a
// scored thinking variant to a runtime request. Keep arenaModelIdentity strict.
func arenaPublicModelIdentity(model string) string {
	model = strings.TrimSpace(model)
	if end := strings.LastIndex(model, "("); end >= 0 && strings.HasSuffix(model, ")") {
		var remaining []string
		removed := false
		for _, part := range strings.Split(model[end+1:len(model)-1], ",") {
			part = strings.TrimSpace(part)
			mode := strings.ToLower(strings.ReplaceAll(part, "_", " "))
			switch mode {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max", "adaptive",
				"thinking", "non-thinking", "non thinking", "no thinking":
				removed = true
			default:
				// Dates, Preview, Pro/Mini and other model qualifiers remain.
				remaining = append(remaining, part)
			}
		}
		if removed {
			model = strings.TrimSpace(model[:end])
			if len(remaining) > 0 {
				model += " (" + strings.Join(remaining, ",") + ")"
			}
		}
	}
	// Normalize explicitly known date spellings without stripping other dates.
	return arenaVersionedIdentity(model)
}

// Official rolling aliases were verified against DeepSeek's model documentation
// on 2026-11-11. This establishes release identity, not a measured score.
// Any routing reference to an older version must be separately marked inferred.
// Third-party gateways do not inherit this promise about the underlying version.
func arenaOfficialDeepSeekVersion(up *Upstream, wire string) (string, bool) {
	if up == nil {
		return wire, false
	}
	base, err := url.Parse(up.BaseURL)
	if err != nil || base.Scheme != "https" || !strings.EqualFold(base.Hostname(), "api.deepseek.com") ||
		(base.Port() != "" && base.Port() != "443") {
		return wire, false
	}
	switch arenaPublicModelIdentity(wire) {
	case "deepseek-flash", "deepseek-v-4-1-flash", "deepseek-v-4-flash", "deepseek-v-4-flash-vision-exp",
		"deepseek-v-4-pro", "deepseek-v-4-pro-vision-exp":
		return "DeepSeek-V4.1-Flash-20261017", true
	default:
		return wire, false
	}
}

type PublicUnrankedSiteModel struct {
	Model    string   `json:"model"`
	Versions []string `json:"versions"`
	Note     string   `json:"note"`
}

type arenaSiteCatalog struct {
	byIdentity map[string][]string
	versions   map[string][]string
	official   map[string]bool
}

func arenaSiteModelIndex(snapshot *routingSnapshot, catalog map[string]string) arenaSiteCatalog {
	index := arenaSiteCatalog{byIdentity: map[string][]string{}, versions: map[string][]string{}, official: map[string]bool{}}
	addUnique := func(items []string, item string) []string {
		for _, existing := range items {
			if existing == item {
				return items
			}
		}
		return append(items, item)
	}
	for model := range catalog {
		for i := range snapshot.upstreams {
			up := &snapshot.upstreams[i]
			if !up.Enabled || up.Weight <= 0 || !servesRouterCandidate(up, model) {
				continue
			}
			wire := up.mapModel(resolveUpstreamCandidateModel(up, model))
			version, official := arenaOfficialDeepSeekVersion(up, wire)
			key := arenaPublicModelIdentity(version)
			if key == "" {
				continue
			}
			// A client alias is not independent evidence of model identity:
			// the actual mapping, including officially redirected aliases, wins.
			index.byIdentity[key] = addUnique(index.byIdentity[key], model)
			// An arbitrary mapped deployment ID may be intentionally private.
			// Only publish already-public client IDs or verified public versions.
			publicVersion := model
			if official {
				publicVersion = version
			}
			index.versions[model] = addUnique(index.versions[model], publicVersion)
			index.official[model] = index.official[model] || official
		}
	}
	for _, models := range index.byIdentity {
		sort.Strings(models)
	}
	for _, versions := range index.versions {
		sort.Strings(versions)
	}
	return index
}

func deepSeekFlashReleaseFamily(key string) string {
	const prefix = "deepseek-v-4-1-flash-"
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	date := strings.TrimPrefix(key, prefix)
	if len(date) != 8 {
		return ""
	}
	for _, r := range date {
		if !unicode.IsDigit(r) {
			return ""
		}
	}
	return prefix
}

func (index arenaSiteCatalog) relatedVersions(source string) []string {
	key := arenaPublicModelIdentity(source)
	family := deepSeekFlashReleaseFamily(key)
	if family == "" {
		return nil
	}
	related := map[string]bool{}
	for identity, models := range index.byIdentity {
		if identity != key && deepSeekFlashReleaseFamily(identity) == family {
			for _, model := range models {
				related[model] = true
			}
		}
	}
	var models []string
	for model := range related {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (index arenaSiteCatalog) unranked(matched map[string]bool) []PublicUnrankedSiteModel {
	result := []PublicUnrankedSiteModel{}
	for model, versions := range index.versions {
		if matched[model] {
			continue
		}
		note := "本站已接入，但当前分榜没有对应型号/版本；不借用其他型号或旧版本的分数。"
		if index.official[model] {
			note = "官方 API 别名对应 V4.1-Flash-1017（2026-10-17，2026-11-11 核验）；当前分榜没有该版本，不套用 0910 或旧 V4 的成绩。"
		}
		result = append(result, PublicUnrankedSiteModel{Model: model, Versions: versions, Note: note})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Model < result[j].Model })
	return result
}
