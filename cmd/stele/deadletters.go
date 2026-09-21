package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/stele"
	"golang.org/x/term"
)

// dead-letters 子命令：Stele 本地死信的管理面（ADR-0011「超限/超龄进入
// 死信并保留原文可重放」）。直开本地 SQLite，不经任何网络面；重放把
// 事件按原凭据（或显式指定的新凭据）重新入队，由正常转发循环投递。

// payloadPreviewBytes 是列表默认载荷预览的字节上限；超长以 … 截断。
const payloadPreviewBytes = 160

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

// loadDeadLetterConfig 解码并校验与网关同一份 component.yaml；管理面只
// 消费 dataDirectory，其余字段的存在性由网关启动自己保证。
func loadDeadLetterConfig(configPath string) (contract.SteleConfig, error) {
	var config contract.SteleConfig
	if err := contract.DecodeFile(configPath, &config); err != nil {
		return config, err
	}
	if config.Component != "stele" {
		return config, fmt.Errorf("configuration component must be stele")
	}
	if config.DataDirectory == "" {
		return config, fmt.Errorf("configuration dataDirectory is required for the local state queue")
	}
	return config, nil
}

// openDeadLetterQueueReadOnly 供 list：mode=ro + query_only 的只读句柄，
// 保证 dataDirectory 配错时绝不创建目录、建表、迁移或 chmod。
func openDeadLetterQueueReadOnly(configPath string) (*stele.Queue, error) {
	config, err := loadDeadLetterConfig(configPath)
	if err != nil {
		return nil, err
	}
	return stele.OpenQueueReadOnly(config.DataDirectory)
}

// openDeadLetterQueueWritable 供 replay（变更操作，允许 bootstrap 与 v1→v2
// 原地迁移）。开库前先确认 stele.db 确实存在：错误 dataDirectory 不得静默
// 新建空库再把所有 id 报成 not found。
func openDeadLetterQueueWritable(configPath string) (*stele.Queue, error) {
	config, err := loadDeadLetterConfig(configPath)
	if err != nil {
		return nil, err
	}
	databasePath := filepath.Join(config.DataDirectory, "stele.db")
	if _, err := os.Stat(databasePath); err != nil {
		return nil, fmt.Errorf("stele: inspect state database %s: %w", databasePath, err)
	}
	return stele.OpenQueue(config.DataDirectory)
}

func deadLettersList(args []string) error {
	flags := flag.NewFlagSet("dead-letters list", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	source := flags.String("source", "", "filter by source kind (e.g. alertmanager)")
	reason := flags.String("reason", "", "filter by reason (rejected|exhausted)")
	limit := flags.Int("limit", 50, "max rows (<=500)")
	full := flags.Bool("full-payload", false, "print the full payload instead of a 160-byte preview (byte-exact when redirected to a file)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	queue, err := openDeadLetterQueueReadOnly(*configPath)
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
	toTerminal := term.IsTerminal(int(os.Stdout.Fd()))
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tSOURCE\tREASON\tATTEMPTS\tFIRST_RECEIVED\tDEAD_AT\tPAYLOAD")
	for _, letter := range letters {
		payload := payloadForDisplay(letter.Payload, *full, toTerminal)
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

// payloadForDisplay 渲染死信原文的输出形态。载荷来自外部告警源、可被构造，
// 终端输出（默认预览与 --full-payload）一律先中和控制字符：原始 ESC/OSC
// 序列不得进入终端，\n/\t 也会破坏表格式。只有把 --full-payload 重定向到
// 文件/管道（非终端 stdout）时才输出逐字节原文，供留档比对。
func payloadForDisplay(payload []byte, full, toTerminal bool) string {
	if full && !toTerminal {
		return string(payload)
	}
	rendered := sanitizeControlChars(string(payload))
	if full || len(rendered) <= payloadPreviewBytes {
		return rendered
	}
	return truncateRunes(rendered, payloadPreviewBytes) + "…"
}

// sanitizeControlChars 把 C0/C1 控制字符与 DEL 替换为空格（按解码后的
// rune 判断，UTF-8 编码的 C1 一样被中和；非法字节经 range 迭代自然落为
// U+FFFD 替换符）。无控制字符时原样返回，避免常见 JSON 载荷多走一遍拷贝。
func sanitizeControlChars(value string) string {
	found := false
	for _, r := range value {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			found = true
			break
		}
	}
	if !found {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			out.WriteByte(' ')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// truncateRunes 在字节上限内回退到 UTF-8 边界，避免把多字节字符切成乱码。
func truncateRunes(value string, limitBytes int) string {
	if limitBytes <= 0 {
		return ""
	}
	if limitBytes >= len(value) {
		return value
	}
	cut := limitBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
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
	ids, err := splitDeadLetterIDs(*idsFlag)
	if err != nil {
		return err
	}
	if *credentialID < 0 {
		return fmt.Errorf("--credential-id must be a positive credential id (got %d)", *credentialID)
	}
	if (*credentialID == 0) != (*snapshotVersion == 0) {
		return fmt.Errorf("--credential-id and --snapshot-version must be given together (or neither, to reuse the original credential)")
	}
	queue, err := openDeadLetterQueueWritable(*configPath)
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

// splitDeadLetterIDs 解析逗号分隔的 id 列表；空串/全空白直接报错而不是
// 静默空操作（操作者显然想重放点什么）。
func splitDeadLetterIDs(raw string) ([]string, error) {
	var ids []string
	for _, id := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("--ids is required (comma-separated dead letter ids from `stele dead-letters list`)")
	}
	return ids, nil
}
