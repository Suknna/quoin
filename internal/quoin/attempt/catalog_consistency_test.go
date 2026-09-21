package attempt

// 跨端冻结目录渲染一致性测试（批准计划第 8 组）：共享冻结 fixture
// （internal/contract/testdata）由两端各自的「真实消费者」入口渲染——本侧是
// CatalogFromInputDocument + FrozenCatalog.ProviderToolsJSON/Digest（创建与
// BeginModelCall 摘要复验走的正是这条链）；Plinth worker 侧是
// ProviderToolsJSONForInput/ProviderToolsDigestForInput。两端互不 import
// （ADR-0011 编译级隔离），因此用同一 fixture + 两侧钉死的同一 digest 字面量
// 把「字节逐一致」从纪律约定变成测试失败。渲染字节形状的变更从此必须是有
// 意识的契约变更：golden 文件与两枚 digest 钉子会同时红。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// pinnedCrossEndProviderToolsDigest 冻结共享 fixture 的 provider schema 摘要。
// internal/plinth/worker 的孪生测试钉住同一字面量——改其一必须同时改两处与
// golden 文件，即为一次显式的跨端契约变更。
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

// TestFrozenCatalogRendersSharedCrossEndFixtureBytes 用共享 fixture 钉住本端
// 渲染字节与摘要：与 golden 文件逐字节相等，摘要与两侧共同的钉子相等。
func TestFrozenCatalogRendersSharedCrossEndFixtureBytes(t *testing.T) {
	input := crossEndFixture(t, "frozen_tool_catalog_input.json")
	golden := crossEndFixture(t, "frozen_tool_catalog_provider_tools.json")
	catalog, ok := CatalogFromInputDocument(input)
	if !ok {
		t.Fatal("shared fixture must embed a frozen tool catalog")
	}
	rendered, err := catalog.ProviderToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rendered, golden) {
		t.Fatalf("quoin rendering drifted from the shared golden bytes (%d vs %d bytes): the worker's BeginModelCall digest would be rejected", len(rendered), len(golden))
	}
	digest, err := catalog.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != pinnedCrossEndProviderToolsDigest {
		t.Fatalf("quoin digest %s != pinned cross-end digest %s", digest, pinnedCrossEndProviderToolsDigest)
	}
	// golden 文件自身必须与钉住的摘要自洽（fixture 内部一致性）。
	sum := sha256.Sum256(golden)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("pinned digest does not match the golden bytes' SHA-256")
	}
}
