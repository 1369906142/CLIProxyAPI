package gateway

import (
	"fmt"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen   string             `yaml:"listen" json:"listen"`
	Backend  BackendConfig      `yaml:"backend" json:"backend"`
	Redis    config.RedisConfig `yaml:"redis" json:"redis"`
	Tenants  []TenantConfig     `yaml:"tenants" json:"tenants"`
	Timeouts TimeoutConfig      `yaml:"timeouts" json:"timeouts"`
}

type BackendConfig struct {
	URL            string `yaml:"url" json:"url"`
	InternalAPIKey string `yaml:"internal-api-key" json:"internal-api-key"`
}

type TenantConfig struct {
	ID        string      `yaml:"id" json:"id"`
	APIKey    string      `yaml:"api-key" json:"api-key"`
	QPS       float64     `yaml:"qps" json:"qps"`
	Burst     int         `yaml:"burst" json:"burst"`
	TimeoutMS int         `yaml:"timeout-ms" json:"timeout-ms"`
	ProxyPool string      `yaml:"proxy-pool" json:"proxy-pool"`
	Retry     RetryConfig `yaml:"retry" json:"retry"`
}

type RetryConfig struct {
	MaxAttempts int `yaml:"max-attempts" json:"max-attempts"`
}

type TimeoutConfig struct {
	ConnectMS        int `yaml:"connect-ms" json:"connect-ms"`
	ResponseHeaderMS int `yaml:"response-header-ms" json:"response-header-ms"`
	TotalMS          int `yaml:"total-ms" json:"total-ms"`
	StreamIdleMS     int `yaml:"stream-idle-ms" json:"stream-idle-ms"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) ApplyDefaults() {
	if c == nil {
		return
	}
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = ":8320"
	}
	c.Backend.URL = strings.TrimSpace(c.Backend.URL)
	c.Backend.InternalAPIKey = strings.TrimSpace(c.Backend.InternalAPIKey)
	c.Redis.Addr = strings.TrimSpace(c.Redis.Addr)
	c.Redis.Password = strings.TrimSpace(c.Redis.Password)
	c.Redis.KeyPrefix = strings.TrimSpace(c.Redis.KeyPrefix)
	if c.Timeouts.ConnectMS <= 0 {
		c.Timeouts.ConnectMS = 5000
	}
	if c.Timeouts.ResponseHeaderMS <= 0 {
		c.Timeouts.ResponseHeaderMS = 30000
	}
	if c.Timeouts.TotalMS <= 0 {
		c.Timeouts.TotalMS = 120000
	}
	if c.Timeouts.StreamIdleMS <= 0 {
		c.Timeouts.StreamIdleMS = 45000
	}
	for i := range c.Tenants {
		tenant := &c.Tenants[i]
		tenant.ID = strings.TrimSpace(tenant.ID)
		tenant.APIKey = strings.TrimSpace(tenant.APIKey)
		tenant.ProxyPool = strings.TrimSpace(tenant.ProxyPool)
		if tenant.QPS <= 0 {
			tenant.QPS = 1
		}
		if tenant.Burst <= 0 {
			tenant.Burst = 1
		}
		if tenant.TimeoutMS <= 0 {
			tenant.TimeoutMS = c.Timeouts.TotalMS
		}
		if tenant.Retry.MaxAttempts <= 0 {
			tenant.Retry.MaxAttempts = 3
		}
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("gateway config is nil")
	}
	if strings.TrimSpace(c.Backend.URL) == "" {
		return fmt.Errorf("gateway backend.url is required")
	}
	if strings.TrimSpace(c.Backend.InternalAPIKey) == "" {
		return fmt.Errorf("gateway backend.internal-api-key is required")
	}
	if strings.TrimSpace(c.Redis.Addr) == "" {
		return fmt.Errorf("gateway redis.addr is required")
	}
	if len(c.Tenants) == 0 {
		return fmt.Errorf("gateway tenants are required")
	}
	seen := make(map[string]struct{}, len(c.Tenants))
	seenAPIKeys := make(map[string]struct{}, len(c.Tenants))
	for _, tenant := range c.Tenants {
		if tenant.ID == "" {
			return fmt.Errorf("gateway tenant id is required")
		}
		if tenant.APIKey == "" {
			return fmt.Errorf("gateway tenant %s api-key is required", tenant.ID)
		}
		if _, ok := seen[tenant.ID]; ok {
			return fmt.Errorf("gateway tenant %s duplicated", tenant.ID)
		}
		seen[tenant.ID] = struct{}{}
		if _, ok := seenAPIKeys[tenant.APIKey]; ok {
			return fmt.Errorf("gateway tenant api-key duplicated for %s", tenant.ID)
		}
		seenAPIKeys[tenant.APIKey] = struct{}{}
	}
	return nil
}
