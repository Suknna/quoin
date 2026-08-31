package helm

import (
	"fmt"
	"strings"
)

// The helper owns retained state Kubernetes objects. The Helm chart never
// renders them, so `helm uninstall` removes workloads while persistent state
// survives for a safe reinstall (OPS-HELM retention semantics).

const (
	retainedSuffixData    = "quoin-data"
	retainedSuffixBackups = "quoin-backups"
	retainedSuffixPlinth  = "plinth-state"
	retainedSuffixLintel  = "lintel-state"
	stagingSuffix         = "bootstrap-staging"
)

// renderRetainedPVCs renders the four retained persistent volume claims.
func renderRetainedPVCs(release string, input installInput) string {
	pvc := func(name string, value pvcInput) string {
		class := ""
		if value.StorageClassName != "" {
			class = "\n  storageClassName: " + value.StorageClassName
		}
		return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %s}
spec:
  accessModes: [%s]
  resources: {requests: {storage: %s}}%s
`, name, release, value.AccessMode, value.Capacity, class)
	}
	return strings.Join([]string{
		pvc(release+"-"+retainedSuffixData, input.Storage.QuoinData),
		pvc(release+"-"+retainedSuffixBackups, input.Storage.QuoinBackup),
		pvc(release+"-"+retainedSuffixPlinth, input.Storage.PlinthState),
		pvc(release+"-"+retainedSuffixLintel, input.Storage.LintelState),
	}, "---\n")
}

// retainedPVCNames lists the retained claim names in fixed order.
func retainedPVCNames(release string) []string {
	return []string{
		release + "-" + retainedSuffixData,
		release + "-" + retainedSuffixBackups,
		release + "-" + retainedSuffixPlinth,
		release + "-" + retainedSuffixLintel,
	}
}

// renderSecretBootstrap renders the disposable one-shot secret bootstrap: a
// staging PVC, the bootstrap configuration (secret material targets /stage)
// and the Job that runs the frozen `quoin secrets bootstrap` path. The
// bootstrap Job has backoffLimit 0; the helper owns retry by re-creating it.
// A fixed root init container prepares volume ownership: the docker runtime
// does not implement fsGroup, so local-path volumes arrive root-owned and the
// non-root bootstrap could neither chmod nor write its staging directory.
func renderSecretBootstrap(release string, input installInput, quoinImage, plinthImage string) string {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[1]s-%[2]s
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-bootstrap-config
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s}
data:
  component.yaml: |
    component: quoin
    publicOrigin: %[3]s
    dataDirectory: /var/lib/quoin/data
    backupDirectory: /var/lib/quoin/backups
    rootKeyFile: /stage/root-key
    runtimeTlsCertificateFile: /stage/runtime-tls.crt
    runtimeTlsPrivateKeyFile: /stage/runtime-tls.key
    steleServiceTokenFile: /stage/stele-service-token
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: %[1]s-secret-bootstrap
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %[1]s-secret-bootstrap
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["%[1]s-secrets"]
    verbs: ["get", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %[1]s-secret-bootstrap
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: %[1]s-secret-bootstrap}
subjects:
  - {kind: ServiceAccount, name: %[1]s-secret-bootstrap}
---
apiVersion: batch/v1
kind: Job
metadata:
  name: %[1]s-secret-bootstrap
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s}
spec:
  backoffLimit: 0
  template:
    metadata:
      labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s, quoin.io/role: secret-bootstrap}
    spec:
      restartPolicy: Never
      serviceAccountName: %[1]s-secret-bootstrap
      automountServiceAccountToken: true
      terminationGracePeriodSeconds: 30
      securityContext: {runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
      initContainers:
        - name: volume-permissions
          image: %[6]s
          imagePullPolicy: IfNotPresent
          command: ["/bin/sh", "-c", "chown 65532:65532 /stage /var/lib/quoin/data && chmod 0700 /stage"]
          securityContext: {runAsNonRoot: false, runAsUser: 0, allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"], add: ["CHOWN", "FOWNER"]}}
          volumeMounts:
            - {name: stage, mountPath: /stage}
            - {name: data, mountPath: /var/lib/quoin/data}
      containers:
        - name: bootstrap
          image: %[4]s
          imagePullPolicy: IfNotPresent
          command: ["/quoin"]
          args: ["secrets", "bootstrap", "--config", "/etc/quoin/component.yaml", "--kubernetes-secret", "%[1]s-secrets"]
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
          volumeMounts:
            - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
            - {name: data, mountPath: /var/lib/quoin/data}
            - {name: stage, mountPath: /stage}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: config
          configMap: {name: %[1]s-bootstrap-config}
        - name: data
          persistentVolumeClaim: {claimName: %[1]s-%[5]s}
        - name: stage
          persistentVolumeClaim: {claimName: %[1]s-%[2]s}
        - {name: tmp, emptyDir: {}}
`, release, stagingSuffix, input.PublicOrigin, quoinImage, retainedSuffixData, plinthImage)
	return manifest
}

// renderExtractPod renders the one-shot extraction pod that streams the staged
// secret files out of the cluster. It uses the Debian-based Plinth image
// because base64 is needed and the distroless Quoin image carries no shell.
func renderExtractPod(release string, plinthImage string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s-secret-extract
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s, quoin.io/role: secret-extract}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 30
  securityContext: {runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: extract
      image: %[2]s
      imagePullPolicy: IfNotPresent
      command: ["sleep", "600"]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
      volumeMounts:
        - {name: stage, mountPath: /stage, readOnly: true}
        - {name: tmp, mountPath: /tmp}
  volumes:
    - name: stage
      persistentVolumeClaim: {claimName: %[1]s-%[3]s}
    - {name: tmp, emptyDir: {}}
`, release, plinthImage, stagingSuffix)
}

// renderAdminBootstrap renders the attached-TTY first administrator pod. The
// container keeps stdin and a TTY open; the helper drives it through
// `kubectl attach -it` under a local pseudo-terminal, mirroring the Compose
// contract. The fixed root init container prepares volume ownership (see
// renderSecretBootstrap).
func renderAdminBootstrap(release, quoinImage, plinthImage, publicOrigin string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-admin-bootstrap-config
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s}
data:
  component.yaml: |
    component: quoin
    publicOrigin: %[4]s
    dataDirectory: /var/lib/quoin/data
    backupDirectory: /var/lib/quoin/backups
    rootKeyFile: /run/quoin-secrets/root-key
    runtimeTlsCertificateFile: /run/quoin-secrets/runtime-tls.crt
    runtimeTlsPrivateKeyFile: /run/quoin-secrets/runtime-tls.key
    steleServiceTokenFile: /run/quoin-secrets/stele-service-token
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s-admin-bootstrap
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %[1]s, quoin.io/role: admin-bootstrap}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 30
  securityContext: {runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  initContainers:
    - name: volume-permissions
      image: %[3]s
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh", "-c", "chown 65532:65532 /var/lib/quoin/data /var/lib/quoin/backups"]
      securityContext: {runAsNonRoot: false, runAsUser: 0, allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"], add: ["CHOWN", "FOWNER"]}}
      volumeMounts:
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: backups, mountPath: /var/lib/quoin/backups}
  containers:
    - name: admin
      image: %[2]s
      imagePullPolicy: IfNotPresent
      stdin: true
      tty: true
      command: ["/quoin"]
      args: ["admin", "create", "--config", "/etc/quoin/component.yaml"]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
      volumeMounts:
        - {name: config, mountPath: /etc/quoin/component.yaml, subPath: component.yaml, readOnly: true}
        - {name: data, mountPath: /var/lib/quoin/data}
        - {name: backups, mountPath: /var/lib/quoin/backups}
        - {name: secrets, mountPath: /run/quoin-secrets, readOnly: true}
        - {name: tmp, mountPath: /tmp}
  volumes:
    - name: config
      configMap: {name: %[1]s-admin-bootstrap-config}
    - name: data
      persistentVolumeClaim: {claimName: %[1]s-%[5]s}
    - name: backups
      persistentVolumeClaim: {claimName: %[1]s-%[6]s}
    - name: secrets
      secret: {secretName: %[1]s-secrets}
    - {name: tmp, emptyDir: {}}
`, release, quoinImage, plinthImage, publicOrigin, retainedSuffixData, retainedSuffixBackups)
}

const bootstrapMarkerState = "complete"

// writeBootstrapComplete writes the non-secret workload gate only after the
// administrator container has explicitly succeeded. The Chart verifies this
// exact object using Helm lookup before it renders a Deployment.
func writeBootstrapComplete(r *runner, stage int, namespace, release string) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s-bootstrap-complete
  labels: {app.kubernetes.io/name: quoin, app.kubernetes.io/instance: %s}
data:
  state: %s
`, release, release, bootstrapMarkerState)
	output, err := r.runInput(stage, "bootstrap-marker-write", manifest, kubectl(namespace, "apply", "--filename", "-")...)
	if err != nil {
		return fmt.Errorf("write bootstrap-complete marker: %s", strings.TrimSpace(output))
	}
	return requireBootstrapComplete(r, stage, namespace, release)
}

// requireBootstrapComplete verifies the fixed helper-owned workload gate.
func requireBootstrapComplete(r *runner, stage int, namespace, release string) error {
	output, err := r.run(stage, "bootstrap-marker-verify", kubectl(namespace, "get", "configmap", release+"-bootstrap-complete", "--output", "jsonpath={.data.state}")...)
	if err != nil || strings.TrimSpace(output) != bootstrapMarkerState {
		return fmt.Errorf("bootstrap-complete marker is absent or invalid (got %q)", strings.TrimSpace(output))
	}
	return nil
}
