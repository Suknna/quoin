import { useEffect, useState } from 'react'
import type { IntakeIssue } from './api'
import { fetchIntakeIssues } from './api'

export function IntakeIssuesList() {
  const [issues, setIssues] = useState<IntakeIssue[]>([])
  const [error, setError] = useState('')

  useEffect(() => {
    void fetchIntakeIssues()
      .then((page) => setIssues(page.items ?? []))
      .catch((reason: unknown) => setError(reason instanceof Error ? reason.message : '接入问题加载失败'))
  }, [])

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
          </div>
        </li>
      ))}
    </ul>
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
