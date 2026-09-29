package main

import (
	"flag"
	"fmt"
	"os"

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
	conf.MustLoad(*configFile, &c, conf.UseEnv())

	svcCtx, err := svc.NewServiceContext(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init service context failed: %v\n", err)
		os.Exit(1)
	}

	server := rest.MustNewServer(c.RestConf)
	defer server.Stop()
	defer svcCtx.Logs.Close()

	handler.RegisterHandlers(server, svcCtx)

	fmt.Printf("Starting llm-relay at %s:%d...\n", c.Host, c.Port)
	server.Start()
}
