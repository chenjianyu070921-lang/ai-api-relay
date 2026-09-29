package svc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"llm-relay/app/llm-relay/internal/config"
	"llm-relay/app/llm-relay/internal/model"
	"llm-relay/app/llm-relay/internal/relay"
)

// ServiceContext 依赖注入容器
type ServiceContext struct {
	Config config.Config

	DB   *gorm.DB
	RDB  redis.UniversalClient
	Bill *relay.Billing
	Logs *relay.LogWriter

	// UpstreamHTTP 上游转发专用 client：连接池 + 独立超时（流式读超时单独控制）
	UpstreamHTTP *http.Client
}

func NewServiceContext(c config.Config) (*ServiceContext, error) {
	s := &ServiceContext{Config: c}

	if c.Mysql.DataSource != "" {
		db, err := model.OpenDB(c.Mysql.DataSource)
		if err != nil {
			return nil, fmt.Errorf("open mysql: %w", err)
		}
		s.DB = db
	}

	if c.Redis.Addr != "" {
		s.RDB = redis.NewClient(&redis.Options{
			Addr:     c.Redis.Addr,
			Password: c.Redis.Password,
		})
	}

	s.Bill = relay.NewBilling(s.DB, s.RDB, c.Relay.QuotaCacheSec)
	s.Logs = relay.NewLogWriter(s.DB, c.Relay.LogBuffer)

	s.UpstreamHTTP = &http.Client{
		Timeout: time.Duration(c.Relay.UpstreamTimeoutMs) * time.Millisecond,
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
			DialContext:         dialerWithTimeout(c.Relay.ConnectTimeoutMs),
		},
	}
	return s, nil
}

func dialerWithTimeout(connectTimeoutMs int) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: time.Duration(connectTimeoutMs) * time.Millisecond}
	return d.DialContext
}
