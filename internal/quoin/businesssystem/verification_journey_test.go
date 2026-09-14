package businesssystem

// Browser-check declarations cannot reach Config Verification until the
// browser declaration compiler is approved. The journey execution harness was
// removed with the retired upload seam; when the compiler is approved, the
// journey lifecycle tests return against the then-active creation path.

import (
	"testing"

	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

const browserCheckSystemYAML = `apiVersion: quoin/v1
kind: BusinessSystem
metadata: {name: payments, displayName: 支付系统, description: 浏览器检查}
spec:
  metrics:
    connectionRef: main-thanos
    matchLabels: {business_system: payments}
    resources:
      - name: browser-resource
        displayName: Browser Resource
        matchLabels: {job: browser}
        discoveryMetric: up
        identityLabels: [instance]
        allowedMetrics: [up]
  alerts: {sourceRefs: [], matchLabels: {}}
  inspections:
    - name: browser-plan
      displayName: 浏览器计划
      schedule: "30 8 * * *"
      timezone: Asia/Shanghai
      checks:
        - name: status-page
          kind: browser
          resourceRef: browser-resource
          expression: up
          question: 状态页是否正常?
`

// TestBrowserDeclarationsStayRejectedUntilCompilerApproved proves the guard
// every retired upload test asserted: a browser-bearing quoin/v1 declaration
// must fail the production parse/compile seam instead of producing an
// executable draft. Historical drafts seeded by the harness therefore can
// never carry browser checks through the declaration path.
func TestBrowserDeclarationsStayRejectedUntilCompilerApproved(t *testing.T) {
	declaration, fields := quoinconfig.ParseBusinessSystem([]byte(browserCheckSystemYAML), quoinconfig.Limits{})
	if len(fields) == 0 {
		if _, err := quoinconfig.CompileBusinessSystemDocument(declaration); err == nil {
			t.Fatal("browser declarations must be rejected until the browser declaration compiler is approved")
		}
		return
	}
}
