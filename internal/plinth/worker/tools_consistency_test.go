package worker

// 跨端冻结目录渲染一致性测试（批准计划第 8 组）：与 Quoin 侧
// internal/quoin/attempt 的孪生测试共享同一冻结 fixture
// （internal/contract/testdata），两侧各自经「真实消费者」入口渲染并钉住同一
// digest 字面量。Plinth 不 import 任何 Quoin 内部包（ADR-0011 编译级隔离），
// 渲染字节的一致性由 internal/contract 的中立纯函数构造保证，再由本测试与
// 孪生测试对共享 golden 的逐字节断言钉死。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// pinnedCrossEndProviderToolsDigest 与 Quoin 侧孪生测试钉住同一字面量：改其一
// 必须同时改两处与 golden 文件，即为一次显式的跨端契约变更。
const pinnedCrossEndProviderToolsDigest = "99a7efe59d387c41a5b5078ddb6af0b2e238e1c406c99aaff1cfcc5e1e695388"

// crossEndFixture 读取共享 fixture（宽松容忍文件尾换行，正文逐字节权威）。
func crossEndFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "contract", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(body, "\n")
}

// TestProviderToolsRenderSharedCrossEndFixtureBytes 用共享 fixture 钉住 worker
// 渲染字节与摘要：与 golden 文件逐字节相等，摘要与两侧共同的钉子相等——
// BeginModelCall 的 tool_schema_digest 复验在 Quoin 侧才发生，这里提前让漂移
// 在两端同时红。
func TestProviderToolsRenderSharedCrossEndFixtureBytes(t *testing.T) {
	input := crossEndFixture(t, "frozen_tool_catalog_input.json")
	golden := crossEndFixture(t, "frozen_tool_catalog_provider_tools.json")
	rendered, err := ProviderToolsJSONForInput(input, WorkerInvestigationAgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rendered, golden) {
		t.Fatalf("worker rendering drifted from the shared golden bytes (%d vs %d bytes): Quoin BeginModelCall would reject the digest", len(rendered), len(golden))
	}
	digest, err := ProviderToolsDigestForInput(input, WorkerInvestigationAgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if digest != pinnedCrossEndProviderToolsDigest {
		t.Fatalf("worker digest %s != pinned cross-end digest %s", digest, pinnedCrossEndProviderToolsDigest)
	}
	// golden 文件自身必须与钉住的摘要自洽（fixture 内部一致性）。
	sum := sha256.Sum256(golden)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("pinned digest does not match the golden bytes' SHA-256")
	}
}
