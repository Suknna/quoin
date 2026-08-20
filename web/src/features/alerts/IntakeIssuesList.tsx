import { useCallback, useEffect, useState } from 'react'
import { acknowledgeIntakeIssue, ApiLikeError, fetchIntakeIssues, type IntakeIssue } from './api'

const REFRESH_MS = 15_000

interface IntakeIssuesProps {
  isAdmin: boolean
}

// Intake issues are a separate query (CONTEXT 告警接入问题): they are not
// Occurrence lifecycle and carry no SSE change channel. The list refreshes
// on a light timer and after each acknowledgement.
export function IntakeIssuesList({ isAdmin }: IntakeIssuesProps) {
  const [issues, setIssues] = useState<IntakeIssue[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState('')
  const [ackError, setAckError] = useState('')

  const refresh = useCallback(async () => {
    try {
      const page = await fetchIntakeIssues()
      setIssues(page.items ?? [])
      setError('')
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '接入问题加载失败')
    }
  }, [])

  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void refresh()
    }, REFRESH_MS)
    return () => window.clearInterval(timer)
  }, [refresh])

  async function acknowledge(issue: IntakeIssue) {
    setBusy(issue.id)
    setAckError('')
    try {
      await acknowledgeIntakeIssue(issue.id, issue.rowVersion)
      await refresh()
    } catch (reason) {
      if (reason instanceof ApiLikeError && reason.status === 409) {
        setAckError('这条问题的版本已变化，正在刷新最新状态。')
        await refresh()
      } else {
        setAckError(reason instanceof Error ? reason.message : '无法确认接入问题')
      }
    } finally {
      setBusy('')
    }
  }

  if (error) {
    return <div className="inline-status" role="alert"><div><strong>接入问题暂时不可用</strong><p>{error}</p></div></div>
  }
  if (issues.length === 0) {
    return (
      <div className="inline-status">
        <span className="status-dot ok" />
        <div><strong>没有未处理的接入问题</strong><p>身份冲突、指纹不符与截断送达会出现在这里。</p></div>
      </div>
    )
  }
  return (
    <div>
      {ackError && <p className="list-note" role="status">{ackError}</p>}
      <ul className="object-list-items">
        {issues.map((issue) => (
          <li key={issue.id}>
            <div className="object-row static">
              <span className={`intake-kind ${issue.kind}`}>{kindLabel(issue.kind)}</span>
              <span className="object-row-main">
                <strong>已发生 {issue.occurrenceCount} 次</strong>
                <span className="object-row-meta">
                  <time dateTime={issue.lastSeenAt}>{new Date(issue.lastSeenAt).toLocaleString()}</time>
                </span>
              </span>
              {isAdmin && (
                <button
                  className="text-button compact"
                  disabled={busy === issue.id}
                  onClick={() => void acknowledge(issue)}
                >
                  {busy === issue.id ? '正在标记…' : '标记已处理'}
                </button>
              )}
            </div>
          </li>
        ))}
      </ul>
    </div>
  )
}

function kindLabel(kind: string): string {
  switch (kind) {
    case 'identity_conflict': return '身份冲突'
    case 'fingerprint_mismatch': return '指纹不符'
    case 'delivery_truncated': return '送达截断'
    default: return kind
  }
}
