package main

import (
	"path"
	"sort"
	"strings"
)

var applicationSubjects = []string{"frontend", "lintel", "plinth", "quoin", "stele"}

// selectBuildSubjects maps changed source paths to independently publishable
// application images. The two Proto authorities deliberately override every
// narrower mapping: all application artifacts must carry one shared contract
// fingerprint. Caddy is a pinned third-party image and is never rebuilt here.
func selectBuildSubjects(changed []string) []string {
	selected := map[string]bool{}
	for _, file := range changed {
		file = strings.TrimPrefix(file, "./")
		if isProtoAuthority(file) {
			return append([]string(nil), applicationSubjects...)
		}
		if isAllApplicationBuildInput(file) {
			selectAll(selected)
			continue
		}
		if isAllBackendBuildInput(file) {
			selectBackends(selected)
			continue
		}
		if file == "docs/specs/quoin-v1/contracts/openapi.yaml" {
			selected["quoin"] = true
			selected["frontend"] = true
			continue
		}
		if isFrontendAndLintelBuildInput(file) {
			selected["frontend"] = true
			selected["lintel"] = true
			continue
		}
		if strings.HasPrefix(file, "web/") {
			selected["frontend"] = true
			continue
		}
		if strings.HasPrefix(file, "internal/lintel/catalog/") {
			// The Quoin scheduler consumes Lintel's catalog contract too.
			selected["quoin"] = true
			selected["lintel"] = true
			continue
		}
		for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
			if strings.HasPrefix(file, "cmd/"+component+"/") || strings.HasPrefix(file, "internal/"+component+"/") ||
				strings.HasPrefix(file, "deploy/images/"+component+"/") {
				selected[component] = true
				goto nextFile
			}
		}
		// Internal packages are intentionally conservatively treated as shared:
		// Go imports are not reconstructed from changed paths in CI, and a false
		// negative would publish an image compiled against stale shared code.
		if strings.HasPrefix(file, "internal/") {
			selectBackends(selected)
		}
	nextFile:
	}
	if len(selected) == 0 {
		return nil
	}
	result := make([]string, 0, len(selected))
	for component := range selected {
		result = append(result, component)
	}
	sort.Strings(result)
	return result
}

func selectAll(selected map[string]bool) {
	for _, component := range applicationSubjects {
		selected[component] = true
	}
}

func selectBackends(selected map[string]bool) {
	for _, component := range []string{"lintel", "plinth", "quoin", "stele"} {
		selected[component] = true
	}
}

// isAllApplicationBuildInput lists sources used by both backend and frontend
// Docker targets. In particular, the shared Dockerfile and its Caddy YAML
// configuration change the frontend runtime as well as backend build stages.
func isAllApplicationBuildInput(file string) bool {
	return strings.HasPrefix(file, "build/package/") ||
		file == "go.mod" || file == "go.sum"
}

// isAllBackendBuildInput names source inputs available to all Go binaries but
// not copied into the standalone static frontend image.
func isAllBackendBuildInput(file string) bool {
	return strings.HasPrefix(file, "cmd/quoin-healthcheck/") ||
		strings.HasPrefix(file, "internal/buildinfo/") ||
		strings.HasPrefix(file, "internal/ops/") ||
		strings.HasPrefix(file, "internal/gen/contracts/") ||
		strings.HasPrefix(file, "internal/contract/")
}

// isFrontendAndLintelBuildInput covers Node workspace metadata and packages:
// frontend builds install the web workspace, while the Lintel journey runner
// installs the same workspace's production dependencies.
func isFrontendAndLintelBuildInput(file string) bool {
	return file == "package.json" || file == "pnpm-lock.yaml" || file == "pnpm-workspace.yaml" ||
		file == "web/package.json"
}

func isProtoAuthority(file string) bool {
	return file == "docs/specs/quoin-v1/contracts/runtime.proto" ||
		file == path.Join("docs/specs/quoin-v1/contracts/quoin/plinth/worker/v1", "agent_worker.proto")
}
