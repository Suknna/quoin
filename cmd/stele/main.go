package main

import (
	"flag"
	"fmt"
	"os"

	// 装配内置插件（ADR-0011）：alertmanager EventSource 等在 init() 注册进
	// 进程默认插件表，webhook 路由据此解析 /webhook/{source}。
	sharedops "github.com/Suknna/quoin/internal/ops"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
	steleops "github.com/Suknna/quoin/internal/stele/ops"
)

func main() {
	configPath := flag.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	flag.Parse()
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	if err := steleops.Run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "stele:", err)
		os.Exit(1)
	}
}
