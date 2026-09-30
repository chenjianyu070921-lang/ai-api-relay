package config

import "github.com/zeromicro/go-zero/rest"

type Config struct {
	rest.RestConf

	Mysql struct {
		DataSource string // 环境变量 LLM_RELAY_MYSQL_DSN 注入，密码不进仓库
	}

	Redis struct {
		Addr     string // host:port
		Password string
	}

	// Relay 对应 03-API设计.md 第 3 节的 Relay 配置块
	Relay struct {
		MasterKeyEnv           string `json:",default=LLM_RELAY_MASTER_KEY"` // 上游key AES 主密钥(P1 加密落库时用)
		DefaultMaxTokens       int    `json:",default=4096"`                 // 客户端没传 max_tokens 时的预扣费上限
		MaxRetryTimes          int    `json:",default=3"`                    // P0 单渠道暂不生效，P2 路由用
		UpstreamTimeoutMs      int    `json:",default=300000"`               // 流式整体读超时
		ConnectTimeoutMs       int    `json:",default=10000"`
		AutoDisableFailCount   int    `json:",default=3"`
		EnforceIncludeUsage    bool   `json:",default=true"`   // OpenAI兼容上游注入 stream_options.include_usage
		BatchUpdateIntervalSec int    `json:",default=5"`      // 配额增量批量落库周期
		LogBuffer              int    `json:",default=1024"`   // 异步日志 channel 容量
		TokenCacheSec          int    `json:",default=30"`     // 令牌缓存 TTL
		QuotaCacheSec          int    `json:",default=604800"` // 配额缓存 TTL，默认 7d
	}

	// Upstream P0 单渠道写死在配置；P2 迁移到 channel 表 + 内存快照路由
	Upstream struct {
		Name    string
		Type    string `json:",default=openai"` // openai | anthropic
		BaseURL string
		Path    string `json:",default=/v1/chat/completions"` // anthropic 系上游配 /v1/messages
		APIKey  string
		Model   string // 上游模型名；非空时强制替换请求模型名
	}
}
