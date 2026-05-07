package management

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (h *Handler) GetModelPrices(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, gin.H{"model-prices": config.ModelPrices{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"model-prices": config.CloneModelPrices(h.cfg.ModelPrices)})
}

func (h *Handler) PutModelPrices(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	var body struct {
		Value config.ModelPrices `json:"value"`
		Items config.ModelPrices `json:"items"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		if err := json.Unmarshal(data, &body.Value); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}

	prices := body.Value
	if len(prices) == 0 && len(body.Items) > 0 {
		prices = body.Items
	}
	if len(prices) == 0 {
		var direct config.ModelPrices
		if err := json.Unmarshal(data, &direct); err == nil && len(direct) > 0 {
			prices = direct
		}
	}
	prices = config.NormalizeModelPrices(prices)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ModelPrices = prices
	usage.SetClientAPIKeyQuotaModelPrices(prices)
	h.persistLocked(c)
}
