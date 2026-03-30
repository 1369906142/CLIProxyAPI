package management

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	gatewaycfg "github.com/router-for-me/CLIProxyAPI/v6/internal/gateway"
	"gopkg.in/yaml.v3"
)

type commercialConfigPayload struct {
	ProxyURL         string                 `json:"proxy-url"`
	RequestRetry     int                    `json:"request-retry"`
	MaxRetryInterval int                    `json:"max-retry-interval"`
	DisableCooling   bool                   `json:"disable-cooling"`
	Routing          config.RoutingConfig   `json:"routing"`
	Redis            config.RedisConfig     `json:"redis"`
	Transport        config.TransportConfig `json:"transport"`
}

func (h *Handler) GetCommercialConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, commercialConfigPayload{})
		return
	}
	c.JSON(http.StatusOK, commercialConfigPayload{
		ProxyURL:         strings.TrimSpace(h.cfg.ProxyURL),
		RequestRetry:     h.cfg.RequestRetry,
		MaxRetryInterval: h.cfg.MaxRetryInterval,
		DisableCooling:   h.cfg.DisableCooling,
		Routing:          h.cfg.Routing,
		Redis:            normalizeRedisConfig(h.cfg.Redis),
		Transport: config.TransportConfig{
			ProxyPools: normalizeProxyPools(h.cfg.Transport.ProxyPools),
		},
	})
}

func (h *Handler) PutCommercialConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config not initialized"})
		return
	}
	var payload commercialConfigPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body", "message": err.Error()})
		return
	}

	payload.ProxyURL = strings.TrimSpace(payload.ProxyURL)
	normalizedStrategy, ok := normalizeRoutingStrategy(payload.Routing.Strategy)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid routing strategy"})
		return
	}

	h.cfg.ProxyURL = payload.ProxyURL
	h.cfg.RequestRetry = maxInt(payload.RequestRetry, 0)
	h.cfg.MaxRetryInterval = maxInt(payload.MaxRetryInterval, 0)
	h.cfg.DisableCooling = payload.DisableCooling
	h.cfg.Routing.Strategy = normalizedStrategy
	h.cfg.Redis = normalizeRedisConfig(payload.Redis)
	h.cfg.Transport.ProxyPools = normalizeProxyPools(payload.Transport.ProxyPools)

	h.persist(c)
}

func (h *Handler) GetGatewayConfig(c *gin.Context) {
	path, ok := h.resolvedGatewayConfigPath(false)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "gateway config not found"})
		return
	}
	cfg, err := gatewaycfg.LoadConfig(path)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway config invalid", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

func (h *Handler) PutGatewayConfig(c *gin.Context) {
	path, ok := h.resolvedGatewayConfigPath(true)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "gateway config path unavailable"})
		return
	}

	var payload gatewaycfg.Config
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body", "message": err.Error()})
		return
	}
	payload.ApplyDefaults()
	if err := payload.Validate(); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid gateway config", "message": err.Error()})
		return
	}

	data, err := yaml.Marshal(&payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal failed", "message": err.Error()})
		return
	}
	if err = ensureParentDir(path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "prepare path failed", "message": err.Error()})
		return
	}
	if err = WriteConfig(path, data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "path": path})
}

func (h *Handler) GetGatewayConfigYAML(c *gin.Context) {
	path, ok := h.resolvedGatewayConfigPath(false)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "gateway config not found"})
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read failed", "message": err.Error()})
		return
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	_, _ = c.Writer.Write(data)
}

func (h *Handler) PutGatewayConfigYAML(c *gin.Context) {
	path, ok := h.resolvedGatewayConfigPath(true)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "gateway config path unavailable"})
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "empty body"})
		return
	}

	tmpDir := filepath.Dir(path)
	if err = os.MkdirAll(tmpDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "prepare path failed", "message": err.Error()})
		return
	}
	tmpFile, err := os.CreateTemp(tmpDir, "gateway-validate-*.yaml")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err = tmpFile.Write(body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	if err = tmpFile.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}

	if _, err = gatewaycfg.LoadConfig(tmpPath); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_gateway_config", "message": err.Error()})
		return
	}

	if err = WriteConfig(path, body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "path": path})
}

func normalizeRedisConfig(cfg config.RedisConfig) config.RedisConfig {
	return config.RedisConfig{
		Addr:      strings.TrimSpace(cfg.Addr),
		Password:  cfg.Password,
		DB:        maxInt(cfg.DB, 0),
		KeyPrefix: strings.TrimSpace(cfg.KeyPrefix),
	}
}

func normalizeProxyPools(entries []config.ProxyPoolConfig) []config.ProxyPoolConfig {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.ProxyPoolConfig, 0, len(entries))
	for _, entry := range entries {
		normalized := config.ProxyPoolConfig{
			ID:              strings.TrimSpace(entry.ID),
			URL:             strings.TrimSpace(entry.URL),
			Weight:          maxInt(entry.Weight, 0),
			MaxFailures:     maxInt(entry.MaxFailures, 0),
			CooldownSeconds: maxInt(entry.CooldownSeconds, 0),
			Mode:            strings.TrimSpace(entry.Mode),
		}
		if normalized.Weight == 0 {
			normalized.Weight = 1
		}
		if normalized.MaxFailures == 0 {
			normalized.MaxFailures = 3
		}
		if normalized.CooldownSeconds == 0 {
			normalized.CooldownSeconds = 60
		}
		for _, rawURL := range entry.URLs {
			trimmed := strings.TrimSpace(rawURL)
			if trimmed != "" {
				normalized.URLs = append(normalized.URLs, trimmed)
			}
		}
		if normalized.ID == "" {
			continue
		}
		if normalized.URL == "" && len(normalized.URLs) == 0 {
			continue
		}
		if normalized.Mode == "" {
			normalized.Mode = "weighted"
		}
		out = append(out, normalized)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (h *Handler) resolvedGatewayConfigPath(forWrite bool) (string, bool) {
	candidates := []string{
		strings.TrimSpace(os.Getenv("CPA_GATEWAY_CONFIG_PATH")),
		strings.TrimSpace(os.Getenv("GATEWAY_CONFIG_PATH")),
	}
	if h != nil {
		baseDir := filepath.Dir(strings.TrimSpace(h.configFilePath))
		if baseDir != "" && baseDir != "." {
			candidates = append(candidates,
				filepath.Join(baseDir, "gateway.yaml"),
				filepath.Join(baseDir, "gateway.yml"),
				filepath.Join(baseDir, "gateway.example.yaml"),
			)
		}
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
		if forWrite {
			return candidate, true
		}
	}
	return "", false
}

func ensureParentDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty path")
	}
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func maxInt(v int, min int) int {
	if v < min {
		return min
	}
	return v
}
