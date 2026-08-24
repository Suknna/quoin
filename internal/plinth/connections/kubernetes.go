package connections

// Kubernetes read-capability probe (T07): the frozen action set is
// server-version, core discovery, grouped discovery and four
// SelfSubjectAccessReviews in the one deterministically resolved effective
// namespace. The kubeconfig subset (cluster endpoint/CA, context selection,
// token or client-cert auth) is parsed directly; the probe uses only the
// read APIs named by the contract.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Contexts       []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTlsVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			TokenFile             string `yaml:"tokenFile"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
			Username              string `yaml:"username"`
			Password              string `yaml:"password"`
		} `yaml:"user"`
	} `yaml:"users"`
}

type kubernetesClient struct {
	http   *http.Client
	server string
	auth   func(*http.Request)
}

// RunKubernetesProbe executes the frozen kubernetes-read-capabilities-v1
// action set and returns the typed detail; a nil error means every action
// passed (pass_expression over all seven).
func RunKubernetesProbe(ctx context.Context, config KubernetesConfig, secret KubernetesSecret) (KubernetesProbeDetail, error) {
	client, effectiveNamespace, err := resolveKubernetes(config, secret.Kubeconfig)
	// The typed child requires a non-empty effective namespace even for
	// failed probes: unresolvable inputs fall back to the frozen default.
	if effectiveNamespace == "" {
		if config.DefaultNamespace != "" {
			effectiveNamespace = config.DefaultNamespace
		} else {
			effectiveNamespace = "default"
		}
	}
	detail := KubernetesProbeDetail{Kind: "kubernetes", EffectiveNamespace: effectiveNamespace}
	if err != nil {
		return detail, err
	}
	detail.VersionOK = true
	if body, err := kubernetesGet(ctx, client, "/version"); err != nil {
		detail.VersionOK = false
		return detail, fmt.Errorf("读取 server version 失败: %w", err)
	} else {
		var version struct {
			GitVersion string `json:"gitVersion"`
		}
		if err := json.Unmarshal(body, &version); err != nil || version.GitVersion == "" {
			detail.VersionOK = false
			return detail, fmt.Errorf("server version 响应缺少 gitVersion")
		}
	}
	detail.CoreDiscoveryOK = true
	if body, err := kubernetesGet(ctx, client, "/api"); err != nil {
		detail.CoreDiscoveryOK = false
		return detail, fmt.Errorf("读取核心 API discovery 失败: %w", err)
	} else {
		var core struct {
			Versions []string `json:"versions"`
		}
		if err := json.Unmarshal(body, &core); err != nil || len(core.Versions) == 0 {
			detail.CoreDiscoveryOK = false
			return detail, fmt.Errorf("核心 discovery 未返回版本列表")
		}
	}
	detail.GroupedDiscoveryOK = true
	if body, err := kubernetesGet(ctx, client, "/apis"); err != nil {
		detail.GroupedDiscoveryOK = false
		return detail, fmt.Errorf("读取分组 discovery 失败: %w", err)
	} else {
		var groups struct {
			Groups []any `json:"groups"`
		}
		if err := json.Unmarshal(body, &groups); err != nil || len(groups.Groups) == 0 {
			detail.GroupedDiscoveryOK = false
			return detail, fmt.Errorf("分组 discovery 未返回 API 组")
		}
	}
	reviews := []struct {
		field *bool
		verb  string
		res   string
		sub   string
	}{
		{&detail.PodsGetAllowed, "get", "pods", ""},
		{&detail.PodsListAllowed, "list", "pods", ""},
		{&detail.EventsListAllowed, "list", "events", ""},
		{&detail.PodsLogGetAllowed, "get", "pods", "log"},
	}
	for _, review := range reviews {
		allowed, err := selfSubjectAccessReview(ctx, client, effectiveNamespace, review.verb, review.res, review.sub)
		*review.field = allowed
		if err != nil {
			return detail, fmt.Errorf("SSAR %s %s 失败: %w", review.verb, review.res+"/"+review.sub, err)
		}
		if !allowed {
			return detail, fmt.Errorf("SSAR %s %s/%s 未授权", review.verb, review.res, review.sub)
		}
	}
	return detail, nil
}

func resolveKubernetes(config KubernetesConfig, kubeconfigYAML string) (*kubernetesClient, string, error) {
	var parsed kubeconfig
	if err := yaml.Unmarshal([]byte(kubeconfigYAML), &parsed); err != nil {
		return nil, "", fmt.Errorf("kubeconfig 不是合法 YAML: %w", err)
	}
	selected := parsed.CurrentContext
	if config.ContextName != "" {
		selected = config.ContextName
	}
	var contextName, userName, contextNamespace string
	found := false
	for _, entry := range parsed.Contexts {
		if entry.Name == selected {
			contextName = entry.Context.Cluster
			userName = entry.Context.User
			contextNamespace = entry.Context.Namespace
			found = true
			break
		}
	}
	if !found {
		return nil, "", fmt.Errorf("kubeconfig 中不存在 context %q", selected)
	}
	var server, caData string
	var caFile string
	var insecure bool
	clusterFound := false
	for _, cluster := range parsed.Clusters {
		if cluster.Name == contextName {
			server = cluster.Cluster.Server
			caData = cluster.Cluster.CertificateAuthorityData
			caFile = cluster.Cluster.CertificateAuthority
			insecure = cluster.Cluster.InsecureSkipTlsVerify
			clusterFound = true
			break
		}
	}
	if !clusterFound || server == "" {
		return nil, "", fmt.Errorf("kubeconfig context %q 缺少可用的 cluster server", selected)
	}
	var token, clientCert, clientKey, basicUser, basicPassword string
	for _, user := range parsed.Users {
		if user.Name != userName {
			continue
		}
		token = user.User.Token
		clientCert = user.User.ClientCertificateData
		clientKey = user.User.ClientKeyData
		basicUser, basicPassword = user.User.Username, user.User.Password
		break
	}
	// Namespace resolution: defaultNamespace -> current context -> default
	// (frozen order).
	effective := config.DefaultNamespace
	if effective == "" {
		effective = contextNamespace
	}
	if effective == "" {
		effective = "default"
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		tlsConfig.InsecureSkipVerify = true
	}
	if caData != "" {
		der, err := base64.StdEncoding.DecodeString(caData)
		if err != nil {
			return nil, "", fmt.Errorf("kubeconfig certificate-authority-data 无法解码: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(der) {
			return nil, "", fmt.Errorf("kubeconfig CA 证书无法解析")
		}
		tlsConfig.RootCAs = pool
	} else if caFile != "" {
		body, err := readFileAll(caFile)
		if err != nil {
			return nil, "", fmt.Errorf("读取 kubeconfig CA 文件失败: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(body) {
			return nil, "", fmt.Errorf("kubeconfig CA 文件无法解析")
		}
		tlsConfig.RootCAs = pool
	}
	var authorize func(*http.Request)
	switch {
	case token != "":
		authorize = func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+token) }
	case clientCert != "" && clientKey != "":
		certDER, err := base64.StdEncoding.DecodeString(clientCert)
		if err != nil {
			return nil, "", fmt.Errorf("client-certificate-data 无法解码: %w", err)
		}
		keyDER, err := base64.StdEncoding.DecodeString(clientKey)
		if err != nil {
			return nil, "", fmt.Errorf("client-key-data 无法解码: %w", err)
		}
		pair, err := tls.X509KeyPair(certDER, keyDER)
		if err != nil {
			return nil, "", fmt.Errorf("client certificate 无法加载: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{pair}
	case basicUser != "":
		authorize = func(request *http.Request) { request.SetBasicAuth(basicUser, basicPassword) }
	default:
		return nil, "", fmt.Errorf("kubeconfig user %q 未提供可用凭据", userName)
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	client := &kubernetesClient{
		http:   &http.Client{Transport: transport, Timeout: probeTimeout},
		server: strings.TrimSuffix(server, "/"),
		auth:   authorize,
	}
	return client, effective, nil
}

func kubernetesGet(ctx context.Context, client *kubernetesClient, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.server+path, nil)
	if err != nil {
		return nil, err
	}
	if client.auth != nil {
		client.auth(request)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return body, nil
}

// KubernetesReadRequest is the closed read-only action set exposed to the
// investigation supervisor. It deliberately contains no URL, method, header,
// selector, exec or connection locator supplied by the model.
type KubernetesReadRequest struct {
	Operation string
	Namespace string
	Name      string
	Container string
}

// RunKubernetesRead reuses the probe's kubeconfig/TLS/auth primitive to run
// one fixed GET request. The caller executes it independently for each frozen
// grant, so one bound cluster can fail without hiding another's observation.
func RunKubernetesRead(ctx context.Context, config KubernetesConfig, secret KubernetesSecret, request KubernetesReadRequest) ([]byte, error) {
	client, _, err := resolveKubernetes(config, secret.Kubeconfig)
	if err != nil {
		return nil, err
	}
	path := ""
	switch request.Operation {
	case "discovery":
		path = "/api"
	case "pod_list":
		path = "/api/v1/namespaces/" + url.PathEscape(request.Namespace) + "/pods"
	case "pod_get":
		path = "/api/v1/namespaces/" + url.PathEscape(request.Namespace) + "/pods/" + url.PathEscape(request.Name)
	case "events_list":
		path = "/api/v1/namespaces/" + url.PathEscape(request.Namespace) + "/events"
	case "pod_logs":
		path = "/api/v1/namespaces/" + url.PathEscape(request.Namespace) + "/pods/" + url.PathEscape(request.Name) + "/log?container=" + url.QueryEscape(request.Container)
	default:
		return nil, fmt.Errorf("unsupported Kubernetes read operation %q", request.Operation)
	}
	return kubernetesGet(ctx, client, path)
}

func selfSubjectAccessReview(ctx context.Context, client *kubernetesClient, namespace, verb, resource, subresource string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	attributes := map[string]any{
		"namespace": namespace, "verb": verb, "group": "", "resource": resource,
	}
	if subresource != "" {
		attributes["subresource"] = subresource
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec":       map[string]any{"resourceAttributes": attributes},
	})
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.server+"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	if client.auth != nil {
		client.auth(request)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var parsed struct {
		Status struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		} `json:"status"`
	}
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return false, fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	return parsed.Status.Allowed, nil
}
