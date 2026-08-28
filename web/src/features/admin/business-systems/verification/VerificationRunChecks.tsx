import { useCallback, useEffect, useRef, useState } from 'react'
import {
  formatTime,
  getVerificationDetail,
  verificationStateText,
  type VerificationRunDetail,
} from '../api'

// One Config Verification Run's check-level outcome view (T23): every
// plan/check row shows its machine status, the Evidence it produced (or the
// closed gap reason), so the operator understands what passed and why a run
// holds or failed — no raw JSON, no internal identifiers beyond run/evidence
// ids that are directly linkable in the product.

const checkStatusText: Record<string, string> = {
  ok: '通过',
  error: '错误',
  gap: '缺口',
}

const gapReasonText: Record<string, string> = {
  runtime_unavailable: '浏览器运行时不可用',
  authentication_required: '浏览器身份未登录',
  authentication_probe_unavailable: '登录状态探测不可用',
  identity_busy: '浏览器身份被占用',
  artifact_commit_failed: '诊断记录提交失败',
  journey_failed: 'Journey 步骤失败',
  query_failed: '查询失败',
  partial_response: '部分响应',
  no_data: '无数据',
  cancelled: '已取消',
  interrupted: '已中断',
}

interface VerificationRunChecksProps {
  systemKey: string
  versionId: string
  runId: string
  onRunUpdated?: (run: VerificationRunDetail) => void
}

export function VerificationRunChecks({ systemKey, versionId, runId, onRunUpdated }: VerificationRunChecksProps) {
  const [detail, setDetail] = useState<VerificationRunDetail | null>(null)
  const [error, setError] = useState('')
  const requestRef = useRef(0)

  const load = useCallback(() => {
    const request = ++requestRef.current
    void getVerificationDetail(systemKey, versionId, runId)
      .then((value) => {
        if (request !== requestRef.current) return
        setDetail(value)
        setError('')
        onRunUpdated?.(value)
      })
      .catch((reason: unknown) => {
        if (request !== requestRef.current) return
        setError(reason instanceof Error ? reason.message : '暂时无法读取验证 Run 详情。')
      })
  }, [systemKey, versionId, runId, onRunUpdated])

  // The parent remounts this component per run (keyed by run id), so a run
  // switch starts from a clean first render instead of a reset effect.
  useEffect(() => {
    load()
    return () => {
      requestRef.current += 1
    }
  }, [load])

  // An active run keeps refreshing so waiting-for-capacity and running
  // journeys surface their real outcome without a manual reload.
  useEffect(() => {
    if (detail?.state !== 'Queued' && detail?.state !== 'Running') return
    const timer = window.setTimeout(load, 2000)
    return () => window.clearTimeout(timer)
  }, [detail, load])

  if (error) {
    return (
      <p className="field-error" role="alert">
        {error}
      </p>
    )
  }
  if (!detail) {
    return <p className="inline-status">正在读取检查结果…</p>
  }

  const active = detail.state === 'Queued' || detail.state === 'Running'
  return (
    <div className="verification-checks">
      <div className="verification-checks-meta">
        <span className={`status-pill ${detail.state === 'Passed' ? 'ok' : active ? 'waiting' : 'muted'}`}>
          {verificationStateText[detail.state]}
        </span>
        {detail.evidenceAt && (
          <span>
            采证时间 <time dateTime={detail.evidenceAt}>{formatTime(detail.evidenceAt)}</time>
          </span>
        )}
        {active && <span>浏览器 Journey 等待容量或执行中，Run 保持运行并自动刷新。</span>}
        {detail.resultDetail && <span>{detail.resultDetail}</span>}
      </div>
      {detail.checkResults.length === 0 ? (
        <p className="detail-muted">{active ? '检查尚未产生结果。' : '该 Run 没有配置检查。'}</p>
      ) : (
        <table className="data-table verification-check-table">
          <thead>
            <tr>
              <th>计划</th>
              <th>检查</th>
              <th>结果</th>
              <th>说明</th>
            </tr>
          </thead>
          <tbody>
            {detail.checkResults.map((result) => (
              <tr key={`${result.planKey}/${result.checkKey}`}>
                <td>{result.planKey}</td>
                <td>{result.checkKey}</td>
                <td>
                  <span className={`status-pill ${result.status === 'ok' ? 'ok' : 'muted'}`}>
                    {checkStatusText[result.status] ?? result.status}
                  </span>
                </td>
                <td>
                  {result.status === 'ok' && result.evidenceId && <span>已留存 Evidence #{result.evidenceId}</span>}
                  {result.gapReason && <span>{gapReasonText[result.gapReason] ?? result.gapReason}</span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
