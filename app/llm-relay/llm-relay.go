package main

import (
	"flag"
	"fmt"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/rest"

	"llm-relay/app/llm-relay/internal/config"
	"llm-relay/app/llm-relay/internal/handler"
	"llm-relay/app/llm-relay/internal/svc"
)

var configFile = flag.String("f", "etc/llm-relay.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	server := rest.MustNewServer(c.RestConf)
	defer server.Stop()

	ctx := svc.NewServiceContext(c)
	handler.RegisterHandlers(server, ctx)

	fmt.Printf("Starting llm-relay at %s:%d...\n", c.Host, c.Port)
	server.Start()
}
