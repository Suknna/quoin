package publish

// The T42 closure helpers: the local Fulcio-shaped signing authority,
// the sixteen subject bundles and the two closure-asset bundles, the
// real offline import proof over the release subjects, the concrete-site
// Deployment Acceptance driver call, and the owned-resource cleanup.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/release/offline"
	"github.com/Suknna/quoin/internal/release/signing"
	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
	deploymentacceptance "github.com/Suknna/quoin/test/release/deployment-acceptance"
)

const (
	closureIdentityPattern = "^https://github\\.com/Suknna/quoin/"
	closureIssuer          = "https://token.actions.githubusercontent.com"
)

// suiteAdminPassword is the release-suite site credential (sentinel).
var suiteAdminPassword = "t42-suite-" + hexOfTime()

// siteAdminPassword is the concrete-site acceptance credential (sentinel).
var siteAdminPassword = "t42-site-" + hexOfTime()

func hexOfTime() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return hex.EncodeToString(sum[:6])
}

// closureSigner mirrors the T39 qualification authority: an ephemeral
// Fulcio-shaped CA issues one code-signing certificate for the release
// workflow identity; every bundle is a real DSSE in-toto statement
// offline-verifiable through the supplychain gate. No key outlives the
// test process.
type closureSigner struct {
	ca       *ecdsa.PrivateKey
	caCert   *x509.Certificate
	leaf     *ecdsa.PrivateKey
	leafCert *x509.Certificate
}

func newClosureSigner(t *testing.T) *closureSigner {
	t.Helper()
	ca, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "quoin-t42-closure-root"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(6 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &ca.PublicKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := "https://github.com/Suknna/quoin/.github/workflows/release.yml@refs/tags/" + releaseVersion42
	identityURL, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	issuerValue, err := asn1.Marshal(closureIssuer)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: identity},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(6 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{identityURL},
		ExtraExtensions: []pkix.Extension{{
			Id:    asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1},
			Value: issuerValue,
		}},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leaf.PublicKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return &closureSigner{ca: ca, caCert: caCert, leaf: leaf, leafCert: leafCert}
}

func (signer *closureSigner) trustRootPath(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "closure-root.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: signer.caCert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signStatement writes one offline-verifiable DSSE bundle over an
// in-toto statement payload bound to one subject digest.
func (signer *closureSigner) signStatement(t *testing.T, dir, name, subjectName, subjectDigest string, payload any) []byte {
	t.Helper()
	statement := map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": predicateTypeOf(payload),
		"subject":       []map[string]any{{"name": subjectName, "digest": map[string]string{"sha256": subjectDigest}}},
		"predicate":     payload,
	}
	encoded, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	paeBytes := supplychain.DSSEPAE("application/vnd.in-toto+json", encoded)
	digest := sha256.Sum256(paeBytes)
	signature, err := ecdsa.SignASN1(rand.Reader, signer.leaf, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	bundle := map[string]any{
		"mediaType": supplychain.BundleMediaType + ";v0.3",
		"verificationMaterial": map[string]any{
			"x590CertificateChain": map[string]any{
				"certificates": []map[string]any{
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.leafCert.Raw)},
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.caCert.Raw)},
				},
			},
		},
		"dsseEnvelope": map[string]any{
			"payloadType": "application/vnd.in-toto+json",
			"payload":     base64.StdEncoding.EncodeToString(encoded),
			"signatures":  []map[string]any{{"keyid": "", "sig": base64.StdEncoding.EncodeToString(signature)}},
		},
	}
	body, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return body
}

func predicateTypeOf(payload any) string {
	if mapping, ok := payload.(map[string]any); ok {
		if _, carries := mapping["kind"]; carries {
			return "https://quoin.dev/release-subject/v1"
		}
	}
	return "https://in-toto.io/attestation/test-result/v0.1"
}

// signSubjectBundles signs the sixteen closed subject assets with the
// inventory's digests (OPS-SUPPLY-002 subject rules).
func signSubjectBundles(t *testing.T, signer *closureSigner, bundlesDir string, inventoryBytes []byte) error {
	t.Helper()
	inventory, err := subjects.Parse(inventoryBytes)
	if err != nil {
		return err
	}
	subject := func(name, digest string) {
		signer.signStatement(t, bundlesDir, inventory.Bundles[name], "quoin-release-subjects/"+name, strings.TrimPrefix(digest, "sha256:"),
			map[string]any{"kind": "release-subject", "asset": name})
	}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		subject("image_indexes/"+component, image.IndexDigest)
		for _, platform := range subjects.Platforms {
			subject("image_manifests/"+component+"/"+platform, image.Platforms[platform])
		}
	}
	subject("helm_oci", inventory.Chart.OCIDigest)
	subject("compose", "sha256:"+inventory.Compose.SHA256)
	for _, platform := range subjects.Platforms {
		subject("deployment_helper/"+platform, "sha256:"+inventory.Helpers[platform].SHA256)
	}
	return nil
}

// signClosureAssets signs the published manifest and the offline archive
// into their two external bundles (one bundle per signed asset).
func signClosureAssets(t *testing.T, recorder *ticketEvidence, signer *closureSigner, releaseDir string, publishedManifest []byte) {
	t.Helper()
	manifestBundle := signing.ManifestBundleName()
	signer.signStatement(t, filepath.Join(releaseDir, "bundles"), manifestBundle,
		"quoin-release-manifest", sha256Hex(publishedManifest),
		map[string]any{"kind": "release-manifest", "release": releaseVersion42})
	offlineName, err := signing.OfflineBundleName(releaseVersion42)
	if err != nil {
		t.Fatal(err)
	}
	archiveBody, err := os.ReadFile(filepath.Join(releaseDir, "quoin-offline-"+releaseVersion42+".tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	signer.signStatement(t, filepath.Join(releaseDir, "bundles"), offlineName,
		"quoin-offline-archive", sha256Hex(archiveBody),
		map[string]any{"kind": "offline-archive", "release": releaseVersion42})
	recorder.observe("closure-assets.json", map[string]any{
		"manifestBundle": manifestBundle,
		"offlineBundle":  offlineName,
		"manifestSHA256": sha256Hex(publishedManifest),
		"archiveSHA256":  sha256Hex(archiveBody),
		"archiveBytes":   len(archiveBody),
	})
}

// proveOfflineImport executes the real subject-level offline import:
// pull each component's index into a layout, verify content addressing,
// import into the fresh registry and read every digest back.
func proveOfflineImport(t *testing.T, recorder *ticketEvidence, workRoot string, inventoryBytes []byte, importRegistry string) map[string]any {
	t.Helper()
	inventory, err := subjects.Parse(inventoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	startImportRegistry(t, recorder, importRegistry)
	runner := offline.ExecRunner{}
	report := map[string]any{"kind": "subject-import", "registry": importRegistry, "components": map[string]any{}}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		layoutDir := filepath.Join(workRoot, "import-layouts", component)
		if err := os.RemoveAll(layoutDir); err != nil {
			t.Fatal(err)
		}
		if err := offline.PullLayout(runner, image.Repository+"@"+image.IndexDigest, layoutDir, true); err != nil {
			t.Fatalf("%s pull: %v", component, err)
		}
		target := importRegistry + "/import/" + component
		if err := offline.ImportLayout(runner, layoutDir, target, true); err != nil {
			t.Fatalf("%s import: %v", component, err)
		}
		digest, platforms, err := offline.ReadBackIndex(runner, target+"@"+image.IndexDigest, true)
		if err != nil {
			t.Fatalf("%s readback: %v", component, err)
		}
		if digest != image.IndexDigest {
			t.Fatalf("%s readback digest %s want %s", component, digest, image.IndexDigest)
		}
		for platform, want := range image.Platforms {
			if platforms[platform] != want {
				t.Fatalf("%s %s readback platform %s want %s", component, platform, platforms[platform], want)
			}
		}
		report["components"].(map[string]any)[component] = map[string]any{
			"indexDigest": digest, "platforms": platforms,
		}
	}
	report["result"] = "imported with digests preserved"
	recorder.observe("subject-offline-import.json", report)
	return report
}

// startImportRegistry provisions one fresh loopback registry container.
func startImportRegistry(t *testing.T, recorder *ticketEvidence, hostPort string) {
	t.Helper()
	if httpReady("http://127.0.0.1:"+portOf(hostPort)+"/v2/", 3*time.Second) {
		return
	}
	name := "t42-import-" + portOf(hostPort)
	recorder.run(t, "registry-pull-"+name, nil, 0, "docker", "pull", "docker.io/library/registry:2")
	reference := strings.TrimSpace(dockerOutputCombined(t, "docker", "image", "inspect", "docker.io/library/registry:2", "--format", "{{index .RepoDigests 0}}"))
	removeDocker(name)
	recorder.run(t, "registry-run-"+name, nil, 0, "docker", "run", "-d", "--name", name, "-p", hostPort+":5000", reference)
	if !httpReady("http://127.0.0.1:"+portOf(hostPort)+"/v2/", 60*time.Second) {
		t.Fatalf("import registry %s did not become ready", hostPort)
	}
}

// runDeploymentAcceptance installs the published release at a concrete
// site through the real helper and drives the acceptance exchange.
func runDeploymentAcceptance(t *testing.T, recorder *ticketEvidence, bin, releaseDir, workRoot string) *deploymentacceptance.Receipt {
	t.Helper()
	siteRoot := filepath.Join(workRoot, "site")
	_ = os.MkdirAll(siteRoot, 0o755)
	secrets := filepath.Join(siteRoot, "secrets")
	_ = os.MkdirAll(secrets, 0o700)
	configPath := filepath.Join(siteRoot, "install.yaml")
	content := fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: 21990\nsteleWebhookHostPort: 21991\nsecretDirectory: %s\nlintelBrowserSlots: 2\nlintelShmSizeBytes: 1073741824\n", secrets)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	project := "t42-site"
	installLog := filepath.Join(recorder.dir, "site-install.log")
	logFile, err := os.OpenFile(installLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	recorder.commands = append(recorder.commands, commandRecord{Name: "site-install", Args: []string{filepath.Join(bin, "compose", "install")}, Log: "site-install.log"})
	reportPath, err := deploymentacceptance.Install(deploymentacceptance.InstallRequest{
		HelperBinary:  filepath.Join(bin, "quoin-deploy"),
		ConfigPath:    configPath,
		ManifestPath:  filepath.Join(releaseDir, "release-manifest.json"),
		WorkRoot:      siteRoot,
		Project:       project,
		AdminPassword: siteAdminPassword,
		QuoinPort:     21990,
		StelePort:     21991,
		Stdout:        logFile, Stderr: logFile,
	})
	if err != nil {
		t.Fatalf("site install failed (see site-install.log, report %s): %v", reportPath, err)
	}

	acceptanceLog := filepath.Join(recorder.dir, "site-acceptance.log")
	acceptanceFile, err := os.OpenFile(acceptanceLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer acceptanceFile.Close()
	recorder.commands = append(recorder.commands, commandRecord{Name: "site-acceptance", Args: []string{filepath.Join(bin, "compose", "verify"), "--helper-request"}, Log: "site-acceptance.log"})
	receipt, err := deploymentacceptance.Run(deploymentacceptance.AcceptanceRequest{
		QuoinPort:       21990,
		AdminPassword:   siteAdminPassword,
		HelperBinary:    filepath.Join(bin, "quoin-deploy"),
		ConfigPath:      configPath,
		WorkRoot:        siteRoot,
		Project:         project,
		ClientCommandID: "t42-site-start-001",
		Stdout:          acceptanceFile, Stderr: acceptanceFile,
	})
	if err != nil {
		t.Fatalf("site acceptance failed (see site-acceptance.log): %v", err)
	}
	recorder.observe("site-receipt.json", receipt)

	// The site stack is this test's own fixture; it comes down with
	// volumes before the cleanup proof.
	downOutput, _ := exec.Command("docker", "compose", "--project-name", project, "down", "--remove-orphans", "--timeout", "45", "-v").CombinedOutput()
	recorder.note("site-down.log", downOutput)
	return receipt
}

// cleanupTicket42 removes every owned resource: the site stack, the
// release suite stack, the registries, the builder and stragglers.
func cleanupTicket42(t *testing.T, recorder *ticketEvidence, workRoot string, builderOwned bool) {
	t.Helper()
	for _, project := range []string{"t42-site"} {
		recorder.run(t, "down-"+project, nil, -1, "docker", "compose", "--project-name", project, "down", "--remove-orphans", "--timeout", "45", "-v")
	}
	// The suite project carries a per-invocation suffix; remove every
	// leftover project by its compose project label (container names
	// carry service suffixes and are not project names).
	seen := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(dockerOutput("ps", "-a", "--format", "{{.Label \"com.docker.compose.project\"}}")), "\n") {
		project := strings.TrimSpace(name)
		if project == "" || seen[project] || !strings.HasPrefix(project, "t42-release-suite") {
			continue
		}
		seen[project] = true
		recorder.run(t, "down-suite-"+project, nil, -1,
			"docker", "compose", "--project-name", project, "down", "--remove-orphans", "--timeout", "45", "-v")
	}
	removeDocker(t42Registry)
	for _, port := range []string{envOr42("QUOIN_T42_IMPORT_PORT", "5143"), envOr42("QUOIN_T42_GATE_PORT", "5144")} {
		removeDocker("t42-import-" + port)
	}
	for _, pattern := range []string{"t42-release"} {
		for _, name := range strings.Split(strings.TrimSpace(dockerOutput("ps", "-a", "--format", "{{.Names}}")), "\n") {
			if strings.HasPrefix(strings.TrimSpace(name), pattern) {
				removeDocker(strings.TrimSpace(name))
			}
		}
	}
	if builderOwned {
		recorder.run(t, "builder-remove", nil, -1, "docker", "buildx", "rm", "-f", t42Builder)
	}
	_ = os.RemoveAll(filepath.Join(workRoot, "finalize"))
	_ = os.RemoveAll(filepath.Join(workRoot, "import-layouts"))
}

// assertOwnedResourceZero42 proves the owned-name space is empty again
// and no baseline resource vanished.
func assertOwnedResourceZero42(t *testing.T, recorder *ticketEvidence, baseline dockerInventory) {
	t.Helper()
	after := captureInventory()
	ownedResidue := []string{}
	for _, name := range strings.Split(strings.TrimSpace(after.Containers), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "t42-") || strings.Contains(name, "t42-site") || strings.Contains(name, "t42-release-suite") {
			ownedResidue = append(ownedResidue, "container:"+name)
		}
	}
	for _, name := range strings.Split(strings.TrimSpace(after.Networks), "\n") {
		name = strings.TrimSpace(name)
		if strings.HasPrefix(name, "t42-") {
			ownedResidue = append(ownedResidue, "network:"+name)
		}
	}
	for _, name := range strings.Split(strings.TrimSpace(after.Volumes), "\n") {
		name = strings.TrimSpace(name)
		if strings.Contains(name, "t42-site") || strings.Contains(name, "t42-release-suite") {
			ownedResidue = append(ownedResidue, "volume:"+name)
		}
	}
	if len(strings.Fields(after.Volumes)) > len(strings.Fields(baseline.Volumes)) {
		_ = exec.Command("docker", "volume", "prune", "-f").Run()
	}
	recorder.observe("owned-resource-zero.json", map[string]any{
		"residue":                ownedResidue,
		"baselineContainersGone": missingCount(baseline.Containers, after.Containers),
	})
	if len(ownedResidue) != 0 {
		t.Fatalf("owned docker residue: %v", ownedResidue)
	}
}

func missingCount(baseline, after string) int {
	present := map[string]bool{}
	for _, entry := range strings.Fields(after) {
		present[entry] = true
	}
	missing := 0
	for _, entry := range strings.Fields(baseline) {
		if !present[entry] {
			missing++
		}
	}
	return missing
}

// cleanupRecord42 assembles the cleanup.json disposition document.
func cleanupRecord42() map[string]any {
	return map[string]any{
		"ownedResources": map[string]string{
			"release-suite-stack":   "compose down -v executed (t42-release-suite project)",
			"site-stack":            "compose down -v executed (t42-site project)",
			"subject-registry":      "docker rm -f t42-registry",
			"import-registries":     "docker rm -f t42-import-5143 / t42-import-5144",
			"buildx-builder":        "docker buildx rm -f (when created by this run)",
			"temporary-credentials": "short-lived site/suite admin passwords, never written to evidence (sentinel scan)",
			"signing-keys":          "ephemeral in-memory Fulcio-shaped CA; nothing persisted",
			"workdirs":              "evidence work tree removed with the evidence directory; finalize/import scratch removed",
			"binfmt-emulation":      "pre-existing host handler, untouched by this run",
		},
		"preExistingUntouched": "baseline inventory snapshot diff proves no foreign container/network/volume was removed",
		"result":               "owned-resource zero; see owned-resource-zero.json",
	}
}
