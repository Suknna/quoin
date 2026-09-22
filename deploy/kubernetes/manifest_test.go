package kubernetes

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestManifestDeliversBrowserFreeStackBehindOneTLSGateway protects the default
// public topology: API/SSE/WebSocket traffic must reach Quoin before the SPA
// fallback, alert intake must reach Stele, and application credentials stay
// outside ConfigMaps. The browser plugin and its Lintel runtime are retired
// (deploy/retired/browser/): the manifest must not contain, reference, or
// provision any Lintel resource.
func TestManifestDeliversBrowserFreeStackBehindOneTLSGateway(t *testing.T) {
	documents := loadDocuments(t, "quoin.yaml")
	deployments := map[string]map[string]any{}
	services := map[string]map[string]any{}
	configMaps := map[string]map[string]any{}
	claims := map[string]map[string]any{}
	for _, document := range documents {
		metadata := document["metadata"].(map[string]any)
		name := metadata["name"].(string)
		switch document["kind"] {
		case "Deployment":
			deployments[name] = document
		case "Service":
			services[name] = document
		case "ConfigMap":
			configMaps[name] = document
		case "PersistentVolumeClaim":
			claims[name] = document
		}
	}

	for _, role := range []string{"gateway", "frontend", "quoin", "plinth", "stele"} {
		if _, found := deployments[role]; !found {
			t.Fatalf("missing %s Deployment", role)
		}
	}
	for _, service := range []string{"gateway", "frontend", "quoin", "stele"} {
		if _, found := services[service]; !found {
			t.Fatalf("missing %s Service", service)
		}
	}
	for _, claim := range []string{"quoin-data", "quoin-backups", "plinth-state", "stele-data"} {
		if _, found := claims[claim]; !found {
			t.Fatalf("missing %s PersistentVolumeClaim", claim)
		}
	}
	// The default stack is browser-free: no Lintel Deployment, ConfigMap,
	// PVC, or Service may appear in the default manifest.
	for name := range deployments {
		if name == "lintel" {
			t.Fatal("default manifest must not deploy Lintel; the browser runtime is retired")
		}
	}
	for name := range configMaps {
		if name == "lintel-config" {
			t.Fatal("default manifest must not ship lintel-config; the browser runtime is retired")
		}
	}
	for name := range claims {
		if name == "lintel-state" {
			t.Fatal("default manifest must not provision lintel-state; the browser runtime is retired")
		}
	}
	for _, exposed := range []string{"plinth", "lintel"} {
		if _, found := services[exposed]; found {
			t.Fatalf("%s must not have a public or cluster service in the default manifest", exposed)
		}
	}
	if rendered := mustMarshal(t, documents); strings.Contains(rendered, "lintel") {
		t.Fatalf("default manifest must not reference Lintel at all: %s", rendered)
	}

	// ADR 0004: the deployment YAML selects the enabled plugin set. The
	// browser-free default stack must enable exactly the three source plugins;
	// the browser and kubernetes plugins are retired and must never be
	// enabled.
	quoinComponent := loadComponentYAML(t, configMaps["quoin-config"]["data"].(map[string]any)["component.yaml"].(string))
	enabled, ok := quoinComponent["enabledPlugins"].([]any)
	if !ok {
		t.Fatalf("quoin-config must declare enabledPlugins: %v", quoinComponent)
	}
	got := make([]string, 0, len(enabled))
	for _, id := range enabled {
		got = append(got, id.(string))
	}
	want := []string{"alertmanager", "prometheus", "thanos"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default enabledPlugins must be exactly %v, got %v", want, got)
	}

	// The alertmanager plugin is useless without a public receiver URL: Quoin
	// answers receiver-config with 503 when stelePublicURL is unset, and the
	// web editor refuses to create any source. The gateway already routes
	// /stele/* to stele:8080, so the manifest must always carry the endpoint.
	stelePublicURL, _ := quoinComponent["stelePublicURL"].(string)
	if !strings.HasPrefix(stelePublicURL, "https://") || !strings.HasSuffix(stelePublicURL, "/stele/webhook/alertmanager") {
		t.Fatalf("quoin-config must set stelePublicURL to the public https receiver endpoint (…/stele/webhook/alertmanager), got %q", stelePublicURL)
	}

	gateway := configMaps["gateway-config"]["data"].(map[string]any)["caddy.json"].(string)
	apiRoute := strings.Index(gateway, `"/api/*"`)
	steleRoute := strings.Index(gateway, `"/stele/*"`)
	frontendRoute := strings.LastIndex(gateway, `"frontend:8080"`)
	if apiRoute < 0 || steleRoute < 0 || frontendRoute < 0 || apiRoute > frontendRoute || steleRoute > frontendRoute {
		t.Fatalf("gateway must route API and Stele before frontend fallback: %s", gateway)
	}
	for _, required := range []string{"quoin:8080", `"/api/*"`, "stele:8080", "Strict-Transport-Security", `"tls_connection_policies": [{}]`, `"automatic_https": {"disable": true, "disable_redirects": true}`, "/etc/caddy/tls/tls.crt"} {
		if !strings.Contains(gateway, required) {
			t.Fatalf("gateway config missing %q", required)
		}
	}
	if deployments["frontend"] == nil || !strings.Contains(mustMarshal(t, deployments["frontend"]), "quoin/frontend:v0.1.0-dev") {
		t.Fatal("frontend must be an independently deployed image")
	}
	for _, secret := range []string{"gateway-tls", "quoin-secrets"} {
		if !strings.Contains(mustMarshal(t, documents), secret) {
			t.Fatalf("manifest must mount deployer-provided %s", secret)
		}
	}
	gatewayContainer := deployments["gateway"]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if command := gatewayContainer["command"].([]any); len(command) != 1 || command[0] != "caddy" {
		t.Fatalf("gateway must explicitly execute stock Caddy: %v", command)
	}
	capabilities := gatewayContainer["securityContext"].(map[string]any)["capabilities"].(map[string]any)
	if additions := capabilities["add"].([]any); len(additions) != 1 || additions[0] != "NET_BIND_SERVICE" {
		t.Fatalf("gateway must retain only Caddy's required file capability: %v", additions)
	}
	for _, role := range []string{"quoin", "plinth", "stele"} {
		if !strings.Contains(mustMarshal(t, deployments[role]), "terminationGracePeriodSeconds: 60") {
			t.Fatalf("%s must preserve the bounded graceful shutdown window", role)
		}
	}
	frontendPod := deployments["frontend"]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	frontendSecurity, ok := frontendPod["securityContext"].(map[string]any)
	if !ok || frontendSecurity["runAsNonRoot"] != true || frontendSecurity["runAsUser"] != 65532 {
		t.Fatal("frontend must explicitly enforce its non-root identity")
	}
	seccomp, ok := frontendSecurity["seccompProfile"].(map[string]any)
	if !ok || seccomp["type"] != "RuntimeDefault" {
		t.Fatal("frontend must enforce the default seccomp profile")
	}
	strategy := deployments["stele"]["spec"].(map[string]any)["strategy"].(map[string]any)
	if strategy["type"] != "Recreate" {
		t.Fatalf("Stele must use Recreate because its relay is single-active: %v", strategy)
	}
}

// volumeMountFields is the closed VolumeMount field set of the Kubernetes
// API. Anything else on a volumeMount entry — above all a volume source such
// as persistentVolumeClaim — is a pod-spec volumes field pasted into the
// wrong list.
var volumeMountFields = map[string]bool{
	"name":              true,
	"mountPath":         true,
	"readOnly":          true,
	"subPath":           true,
	"subPathExpr":       true,
	"mountPropagation":  true,
	"recursiveReadOnly": true,
}

// TestManifestVolumeMountsStayMounts protects the manifest shape that
// `kubectl apply --dry-run=server` enforces: a container volumeMount entry
// may only carry mount fields and must not paste a volume source
// (persistentVolumeClaim, secret, ...) in place of a mountPath — such an
// entry makes the whole apply fail with "does not contain declared merge
// key: mountPath". It also pins ADR-0011: Stele's local runtime state
// (SQLite event queue, dedup, rate counters, token cache, dead letters)
// must be mounted from the stele-data PVC at the dataDirectory configured
// in stele-config.
func TestManifestVolumeMountsStayMounts(t *testing.T) {
	documents := loadDocuments(t, "quoin.yaml")
	steleComponent := map[string]any{}
	for _, document := range documents {
		if document["kind"] != "Deployment" {
			continue
		}
		name := document["metadata"].(map[string]any)["name"].(string)
		podSpec := document["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		volumes := map[string]map[string]any{}
		for _, entry := range toList(t, name, podSpec["volumes"]) {
			volume := toMap(t, name, "volumes entry", entry)
			volumeName, _ := volume["name"].(string)
			volumes[volumeName] = volume
		}
		for _, entry := range toList(t, name, podSpec["containers"]) {
			container := toMap(t, name, "container", entry)
			for _, mountEntry := range toList(t, name, container["volumeMounts"]) {
				mount := toMap(t, name, "volumeMount", mountEntry)
				for field := range mount {
					if !volumeMountFields[field] {
						t.Fatalf("%s volumeMount %v: %q is not a VolumeMount field; volume sources belong under pod volumes", name, mount, field)
					}
				}
				if mountPath, _ := mount["mountPath"].(string); mountPath == "" {
					t.Fatalf("%s volumeMount %v: mountPath is required", name, mount)
				}
				mountName, _ := mount["name"].(string)
				if _, found := volumes[mountName]; !found {
					t.Fatalf("%s volumeMount %v: no pod volume named %q", name, mount, mountName)
				}
			}
		}
	}

	// ADR-0011 regression: the stele-data PVC must be mounted inside the
	// container at the directory stele-config declares as dataDirectory.
	for _, document := range documents {
		metadata, _ := document["metadata"].(map[string]any)
		if document["kind"] == "ConfigMap" && metadata["name"] == "stele-config" {
			data, _ := document["data"].(map[string]any)
			steleComponent = loadComponentYAML(t, data["component.yaml"].(string))
		}
	}
	dataDirectory, _ := steleComponent["dataDirectory"].(string)
	if dataDirectory == "" {
		t.Fatal("stele-config must declare dataDirectory")
	}
	var steleDeployment map[string]any
	for _, document := range documents {
		if document["kind"] == "Deployment" && document["metadata"].(map[string]any)["name"] == "stele" {
			steleDeployment = document
		}
	}
	if steleDeployment == nil {
		t.Fatal("missing stele Deployment")
	}
	podSpec := steleDeployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	mounted := map[string]string{}
	for _, entry := range podSpec["containers"].([]any) {
		for _, mountEntry := range toList(t, "stele", entry.(map[string]any)["volumeMounts"]) {
			mount := toMap(t, "stele", "volumeMount", mountEntry)
			mountName, _ := mount["name"].(string)
			mounted[mountName], _ = mount["mountPath"].(string)
		}
	}
	claim := ""
	for _, entry := range toList(t, "stele", podSpec["volumes"]) {
		volume := toMap(t, "stele", "volumes entry", entry)
		if name, _ := volume["name"].(string); name == "data" {
			if source, ok := volume["persistentVolumeClaim"].(map[string]any); ok {
				claim, _ = source["claimName"].(string)
			}
		}
	}
	if claim != "stele-data" {
		t.Fatalf("stele must back its data volume with the stele-data PVC, got %q", claim)
	}
	if mounted["data"] != dataDirectory {
		t.Fatalf("stele must mount the stele-data PVC at its configured dataDirectory %q, got %q", dataDirectory, mounted["data"])
	}
}

func toList(t *testing.T, deployment string, value any) []any {
	t.Helper()
	if value == nil {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("%s: expected a list, got %T", deployment, value)
	}
	return list
}

func toMap(t *testing.T, deployment string, what string, value any) map[string]any {
	t.Helper()
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s %s: expected a map, got %T", deployment, what, value)
	}
	return m
}

// TestComposeQuoinConfigDefaultsMatchThePluginContract protects the Compose
// side of the same coordination: the default component config enables exactly
// the three source plugins; the browser and kubernetes plugins are retired
// (historical variants in deploy/retired/).
func TestComposeQuoinConfigDefaultsMatchThePluginContract(t *testing.T) {
	defaults := parseConfigFile(t, "../config/quoin.yaml")

	want := []any{"alertmanager", "prometheus", "thanos"}
	if got := defaults["enabledPlugins"]; !equalStrings(got, want) {
		t.Fatalf("config/quoin.yaml must enable exactly %v, got %v", want, got)
	}
}

func parseConfigFile(t *testing.T, path string) map[string]any {
	t.Helper()
	file, err := os.Open(filepath.Join(".", path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(false)
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if document == nil {
		t.Fatalf("%s must contain one YAML document", path)
	}
	var trailing map[string]any
	if err := decoder.Decode(&trailing); err == nil {
		t.Fatalf("%s must contain exactly one YAML document, found at least two", path)
	} else if !errors.Is(err, io.EOF) {
		t.Fatalf("parse %s: %v", path, err)
	}
	return document
}

func equalStrings(got any, want []any) bool {
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for index, value := range list {
		if value != want[index] {
			return false
		}
	}
	return true
}

func loadComponentYAML(t *testing.T, component string) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(component), &parsed); err != nil {
		t.Fatalf("embedded component.yaml must parse: %v", err)
	}
	return parsed
}

func loadDocuments(t *testing.T, name string) []map[string]any {
	t.Helper()
	file, err := os.Open(filepath.Join(".", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	var documents []map[string]any
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents
		}
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, document)
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
