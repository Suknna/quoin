package main

import (
	"flag"
	"fmt"
	"os"

	// 编译期选择插件（ADR-0011）：EventSource 等在 init() 注册进默认表。
	sharedops "github.com/Suknna/quoin/internal/ops"
	steleops "github.com/Suknna/quoin/internal/stele/ops"
	_ "github.com/Suknna/quoin/plugins/alertmanager"
	_ "github.com/Suknna/quoin/plugins/metrics"
)

func main() {
	// 子命令：dead-letters 是本地死信的管理面（直开 SQLite，不启动网关）。
	if len(os.Args) > 1 && os.Args[1] == "dead-letters" {
		if err := runDeadLetters(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "stele dead-letters:", err)
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	flag.Parse()
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	if err := steleops.Run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "stele:", err)
		os.Exit(1)
	}
}
