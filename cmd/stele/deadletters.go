package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/stele"
)

// dead-letters 子命令：Stele 本地死信的管理面（ADR-0011「超限/超龄进入
// 死信并保留原文可重放」）。直开本地 SQLite，不经任何网络面；重放把
// 事件按原凭据（或显式指定的新凭据）重新入队，由正常转发循环投递。

func runDeadLetters(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stele dead-letters <list|replay> [--config path] [options]")
	}
	switch args[0] {
	case "list":
		return deadLettersList(args[1:])
	case "replay":
		return deadLettersReplay(args[1:])
	default:
		return fmt.Errorf("unknown dead-letters action %q (want list|replay)", args[0])
	}
}

func openDeadLetterQueue(configPath string) (*stele.Queue, error) {
	var config contract.SteleConfig
	if err := contract.DecodeFile(configPath, &config); err != nil {
		return nil, err
	}
	if config.Component != "stele" {
		return nil, fmt.Errorf("configuration component must be stele")
	}
	if config.DataDirectory == "" {
		return nil, fmt.Errorf("configuration dataDirectory is required for the local state queue")
	}
	return stele.OpenQueue(config.DataDirectory)
}

func deadLettersList(args []string) error {
	flags := flag.NewFlagSet("dead-letters list", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	source := flags.String("source", "", "filter by source kind (e.g. alertmanager)")
	reason := flags.String("reason", "", "filter by reason (rejected|exhausted)")
	limit := flags.Int("limit", 50, "max rows (<=500)")
	full := flags.Bool("full-payload", false, "print the full payload instead of a 160-byte preview")
	if err := flags.Parse(args); err != nil {
		return err
	}
	queue, err := openDeadLetterQueue(*configPath)
	if err != nil {
		return err
	}
	defer queue.Close()
	letters, err := queue.ListDeadLetters(context.Background(), stele.DeadLetterFilter{
		SourceKind: *source, Reason: *reason, Limit: *limit,
	})
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tSOURCE\tREASON\tATTEMPTS\tFIRST_RECEIVED\tDEAD_AT\tPAYLOAD")
	for _, letter := range letters {
		payload := string(letter.Payload)
		if !*full && len(payload) > 160 {
			payload = payload[:160] + "…"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			letter.ID, letter.SourceKind, letter.Reason, letter.Attempts,
			letter.FirstReceivedAt.Format(time.RFC3339), letter.DeadAt.Format(time.RFC3339), payload)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if len(letters) == 0 {
		fmt.Println("no dead letters")
	}
	return nil
}

func deadLettersReplay(args []string) error {
	flags := flag.NewFlagSet("dead-letters replay", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	idsFlag := flags.String("ids", "", "comma-separated dead letter ids to replay (required)")
	credentialID := flags.Int64("credential-id", 0, "replay with this credential id instead of the original (required after rotation)")
	snapshotVersion := flags.Uint64("snapshot-version", 0, "credential snapshot version matching --credential-id (0 lets Quoin adjudicate)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *idsFlag == "" {
		return fmt.Errorf("--ids is required (comma-separated dead letter ids from `stele dead-letters list`)")
	}
	if (*credentialID == 0) != (*snapshotVersion == 0) {
		return fmt.Errorf("--credential-id and --snapshot-version must be given together (or neither, to reuse the original credential)")
	}
	var ids []string
	for _, id := range strings.Split(*idsFlag, ",") {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	queue, err := openDeadLetterQueue(*configPath)
	if err != nil {
		return err
	}
	defer queue.Close()
	replayed, err := queue.ReplayDeadLetters(context.Background(), ids, *credentialID, *snapshotVersion)
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d dead letter(s) back into the outbox\n", replayed)
	return nil
}
