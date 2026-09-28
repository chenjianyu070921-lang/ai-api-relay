package svc

import "llm-relay/app/llm-relay/internal/config"

// ServiceContext 依赖注入容器，后续在这里挂 db/redis/repository
type ServiceContext struct {
	Config config.Config
}

func NewServiceContext(c config.Config) *ServiceContext {
	return &ServiceContext{
		Config: c,
	}
}
