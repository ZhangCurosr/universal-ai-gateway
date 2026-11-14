package config

import (
	"fmt"
	"time"
)

// Webhook 消息格式（CFG-22，AL-05，T64）。
const (
	WebhookGeneric  = "generic"
	WebhookSlack    = "slack"
	WebhookFeishu   = "feishu"
	WebhookWeCom    = "wecom"
	WebhookDingTalk = "dingtalk"
)

// AlertsConfig 是池级告警的全局设置（T64）。
type AlertsConfig struct {
	CheckInterval time.Duration  `yaml:"check_interval"` // 缺省 1m
	Repeat        time.Duration  `yaml:"repeat"`         // 持续告警的重复推送间隔，缺省 24h
	Webhook       *WebhookConfig `yaml:"webhook"`
}

// WebhookConfig 是告警推送的目标。地址常带令牌，所以只从文件读取。
type WebhookConfig struct {
	URLFile string `yaml:"url_file"`
	Format  string `yaml:"format"` // 缺省 generic
}

// PoolAlerts 是一个池的告警阈值；零值表示不检查该项。
type PoolAlerts struct {
	MinAvailableKeys int           `yaml:"min_available_keys"`
	MinQuotaLeft     int64         `yaml:"min_quota_left"`
	ExpiringWithin   time.Duration `yaml:"expiring_within"`
}

func (a *AlertsConfig) normalize() {
	setDefault(&a.CheckInterval, time.Minute)
	setDefault(&a.Repeat, 24*time.Hour)
	if a.Webhook != nil {
		setDefault(&a.Webhook.Format, WebhookGeneric)
	}
}

// validateAlerts 实现 CFG-22（webhook 文件内容在启动时由装配代码读取并检查）。
func (c *Config) validateAlerts() error {
	a := c.Alerts
	if a.CheckInterval < 10*time.Second {
		return fieldErr("alerts.check_interval", "must be >= 10s, got %s", a.CheckInterval)
	}
	if a.Repeat < time.Minute {
		return fieldErr("alerts.repeat", "must be >= 1m, got %s", a.Repeat)
	}
	if w := a.Webhook; w != nil {
		if w.URLFile == "" {
			return fieldErr("alerts.webhook.url_file", "must not be empty")
		}
		switch w.Format {
		case WebhookGeneric, WebhookSlack, WebhookFeishu, WebhookWeCom, WebhookDingTalk:
		default:
			return fieldErr("alerts.webhook.format", "must be generic, slack, feishu, wecom or dingtalk, got %q", w.Format)
		}
	}
	for i, p := range c.Pools {
		pa := p.Alerts
		if pa == nil {
			continue
		}
		path := fmt.Sprintf("pools[%d].alerts", i)
		if pa.MinAvailableKeys < 0 {
			return fieldErr(path+".min_available_keys", "must be >= 1, got %d", pa.MinAvailableKeys)
		}
		if pa.MinQuotaLeft < 0 {
			return fieldErr(path+".min_quota_left", "must be >= 1, got %d", pa.MinQuotaLeft)
		}
		if pa.MinQuotaLeft > 0 && p.Quota == nil {
			return fieldErr(path+".min_quota_left", "needs pools[%d].quota", i)
		}
		if pa.ExpiringWithin != 0 && pa.ExpiringWithin < time.Hour {
			return fieldErr(path+".expiring_within", "must be >= 1h, got %s", pa.ExpiringWithin)
		}
		if pa.MinAvailableKeys == 0 && pa.MinQuotaLeft == 0 && pa.ExpiringWithin == 0 {
			return fieldErr(path, "set at least one of min_available_keys, min_quota_left, expiring_within")
		}
	}
	return nil
}
