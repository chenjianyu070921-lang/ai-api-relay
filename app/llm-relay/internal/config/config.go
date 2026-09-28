package config

import "github.com/zeromicro/go-zero/rest"

type Config struct {
	rest.RestConf

	// Upstream 上游渠道配置（P0 单渠道写死，P2 迁移到 channel 表）
	Upstream struct {
		Name    string
		BaseURL string
		APIKey  string
	}

	// AuthKey 对外虚拟 key（P0 写死，P3 换成令牌表 + Redis 缓存）
	AuthKey string
}
