package images_test

// 镜像构建上下文守卫：三个 Go 组件镜像（quoin/plinth/stele）的
// Dockerfile.dockerignore 采用白名单语义（`**` 全排除后逐项放行）。新增
// 顶级源码目录而忘记放行时，Docker 的 COPY 会静默丢目录，Go 构建报
// "no required module provides package"，且只在发布构建机上暴露
// （v0.1.1 发布曾因此失败）。本测试把该失败前移到 go test：任何包含
// 非测试 Go 源码的顶级目录必须出现在每个 Go 组件的放行清单里。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleRoot 从测试工作目录向上找到 go.mod 所在的仓库根。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test working directory")
		}
		dir = parent
	}
}

var goImageContexts = []string{"quoin", "plinth", "stele"}

// excludedSourceDirs 是有意不进镜像构建上下文的顶级源码目录：每项都必须
// 附带理由。新增条目是有意识的决定，不足者为失败。
var excludedSourceDirs = map[string]string{
	// 集成测试支撑（密钥/运行时装配），只被 _test 与 e2e 装置引用，
	// 不进任何 cmd 二进制的编译图。
	"test": "integration-test scaffolding, never imported by cmd binaries",
}

// topLevelGoSourceDirs 返回含有至少一个非 _test.go 源文件的顶级目录。
func topLevelGoSourceDirs(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	dirs := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		err := filepath.WalkDir(filepath.Join(root, entry.Name()), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "dist" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				dirs[entry.Name()] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return dirs
}

func TestGoImageContextsIncludeEverySourceDirectory(t *testing.T) {
	sourceDirs := topLevelGoSourceDirs(t)
	if len(sourceDirs) == 0 {
		t.Fatal("no top-level Go source directories found; the guard itself is broken")
	}
	for _, component := range goImageContexts {
		content, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "images", component, "Dockerfile.dockerignore"))
		if err != nil {
			t.Fatalf("component %s: %v", component, err)
		}
		whitelist := map[string]bool{}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "!") && strings.HasSuffix(line, "/") {
				whitelist[strings.TrimSuffix(strings.TrimPrefix(line, "!"), "/")] = true
			}
		}
		for dir := range sourceDirs {
			if _, excluded := excludedSourceDirs[dir]; excluded {
				continue
			}
			if !whitelist[dir] {
				t.Errorf("component %s build context excludes source directory %q (add \"!%s/\" to its Dockerfile.dockerignore)", component, dir, dir)
			}
		}
		for _, required := range []string{"cmd", "internal"} {
			if !whitelist[required] {
				t.Errorf("component %s must always whitelist %q", component, required)
			}
		}
	}
}
