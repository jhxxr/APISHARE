package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

type pricingCatalogItem struct {
	Model        string                  `json:"model"`
	Price        Price                   `json:"price"`
	Source       string                  `json:"source"`
	PricingModel string                  `json:"pricing_model"`
	Connected    bool                    `json:"connected"`
	Variants     []pricingCatalogVariant `json:"variants,omitempty"`
	Override     bool                    `json:"override"`
}

type pricingCatalogVariant struct {
	Price        Price  `json:"price"`
	Source       string `json:"source"`
	PricingModel string `json:"pricing_model"`
}

// A client alias with no own rate is billed using the actual upstream model.
// Show all possible rates when different upstreams map the alias differently.
func aliasPricingVariants(s AppSettings, data map[string]Price, snapshot *routingSnapshot, model string) []pricingCatalogVariant {
	seen := map[string]bool{}
	variants := []pricingCatalogVariant{}
	for i := range snapshot.upstreams {
		u := &snapshot.upstreams[i]
		if !u.Enabled || u.Weight <= 0 || !u.servesModel(model) {
			continue
		}
		candidate := canonicalPricingModel(u.mapModel(model))
		price, source, ok := lookupModelPrice(s, data, candidate)
		if !ok {
			candidate = "default"
			price, ok = s.Pricing[candidate]
			source = "default"
			if !ok {
				price, source, candidate = Price{}, "none", ""
			}
		}
		variant := pricingCatalogVariant{price, source, candidate}
		key, _ := json.Marshal(variant)
		if !seen[string(key)] {
			seen[string(key)] = true
			variants = append(variants, variant)
		}
	}
	sort.Slice(variants, func(i, j int) bool { return variants[i].PricingModel < variants[j].PricingModel })
	return variants
}

func autoPricingInfo(s AppSettings) gin.H {
	autoPricing.RLock()
	defer autoPricing.RUnlock()
	return gin.H{
		"enabled": s.AutoPricingEnabled, "url": s.AutoPricingURL,
		"last_update": autoPricing.fetchedAt, "model_count": autoPricingModelCount(autoPricing.data),
	}
}

func registerPricingAdminRoutes(auth *gin.RouterGroup) {
	auth.GET("/settings/pricing", func(c *gin.Context) {
		snapshot, err := getRoutingSnapshot()
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		connected, _, loading := publicModelCatalogWithDiscovery(snapshot, false)
		s := getSettings()
		autoPricing.RLock()
		models := map[string]bool{"default": true}
		for model := range s.Pricing {
			models[canonicalPricingModel(model)] = true
		}
		for model := range builtInPricing {
			models[model] = true
		}
		for model := range connected {
			models[model] = true
		}
		for model := range autoPricing.data {
			models[strings.TrimPrefix(model, "bare:")] = true
		}
		items := make([]pricingCatalogItem, 0, len(models))
		for model := range models {
			_, isConnected := connected[model]
			candidate := canonicalPricingModel(model)
			_, overridden := s.Pricing[candidate]
			price, source, ok := lookupModelPrice(s, autoPricing.data, candidate)
			var variants []pricingCatalogVariant
			if !ok && isConnected && model != "default" {
				variants = aliasPricingVariants(s, autoPricing.data, snapshot, model)
				if len(variants) == 1 {
					price, source, candidate, ok = variants[0].Price, variants[0].Source, variants[0].PricingModel, true
					variants = nil
				} else if len(variants) > 1 {
					price, source, candidate, ok = Price{}, "upstream", "", true
				}
			}
			if model == "default" || !ok {
				candidate = "default"
				price, ok = s.Pricing["default"]
				source = "default"
				if !ok {
					price, source, candidate = Price{}, "none", ""
				}
			}
			items = append(items, pricingCatalogItem{Model: model, Price: price, Source: source, PricingModel: candidate, Connected: isConnected, Variants: variants, Override: overridden})
		}
		autoPricing.RUnlock()
		sort.Slice(items, func(i, j int) bool {
			if (items[i].Model == "default") != (items[j].Model == "default") {
				return items[i].Model == "default"
			}
			if items[i].Connected != items[j].Connected {
				return items[i].Connected
			}
			if (items[i].Source == "override") != (items[j].Source == "override") {
				return items[i].Source == "override"
			}
			return items[i].Model < items[j].Model
		})
		c.JSON(200, gin.H{"models": items, "pricing": s.Pricing, "auto_pricing": autoPricingInfo(s), "models_loading": loading})
	})

	auth.PUT("/settings/pricing/model", func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
			Price *Price `json:"price"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "请填写有效的模型名称和价格：" + err.Error()})
			return
		}
		model := canonicalPricingModel(strings.TrimSpace(req.Model))
		if model == "" || strings.HasPrefix(model, "bare:") || req.Price == nil || !validPrice(*req.Price) {
			c.JSON(400, gin.H{"error": "模型名称不能为空，单价必须为有限的非负数"})
			return
		}
		if err := updateManualPrice(model, req.Price); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "model": model})
	})

	auth.DELETE("/settings/pricing/model", func(c *gin.Context) {
		model := canonicalPricingModel(strings.TrimSpace(c.Query("model")))
		if model == "" || model == "default" {
			c.JSON(400, gin.H{"error": "请选择模型；默认兜底价格请通过编辑修改"})
			return
		}
		if err := updateManualPrice(model, nil); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
}

// Publish a new immutable map only after persistence succeeds. Row edits update
// only pricing and cannot overwrite unrelated workspace settings or other rows.
func updateManualPrice(model string, price *Price) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	prices := make(map[string]Price, len(settingsCache.Pricing)+1)
	for key, value := range settingsCache.Pricing {
		prices[key] = value
	}
	if price == nil {
		delete(prices, model)
	} else {
		prices[model] = *price
	}
	blob, err := json.Marshal(prices)
	if err != nil {
		return fmt.Errorf("encode pricing: %w", err)
	}
	if err := settingsSet("pricing", string(blob)); err != nil {
		return err
	}
	settingsCache.Pricing = prices
	return nil
}
