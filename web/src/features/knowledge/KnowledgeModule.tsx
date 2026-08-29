import { useEffect, useState } from 'react'
import {
  api, candidateSourceLabels, candidateStateLabels,
  type CandidateSummary, type KnowledgeDetail, type KnowledgeSummary, type KnowledgeVersionSummary,
} from './api'

// KnowledgeModule is the knowledge workbench (UI-KNOWLEDGE-001): the
// second column carries the 知识/待确认 segments; the detail pane shows
// the selected knowledge (current version, source diagnosis pointer and
// the immutable version history) or the pending-candidate list.

// KnowledgeList/CandidatesList/KnowledgeDetailPane are the composable
// panes of the knowledge workbench (UI-KNOWLEDGE-001); the Workbench owns
// the second-column segments and the routing between them.
export function KnowledgeList({ onOpen }: { onOpen: (knowledgeId: string) => void }) {
  const [items, setItems] = useState<KnowledgeSummary[] | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    let cancelled = false
    void api.browse()
      .then((page) => { if (!cancelled) setItems(page.items) })
      .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '知识暂时不可读。') })
    return () => { cancelled = true }
  }, [])
  if (error) return <p className="field-error" role="alert">{error}</p>
  if (!items) return <p className="inline-status">正在读取知识…</p>
  if (items.length === 0) {
    return (
      <p className="detail-muted">
        还没有确认过任何知识。在初步分析、巡检报告或调查消息上使用“整理为知识”，确认后会出现在这里。
      </p>
    )
  }
  return (
    <ul className="knowledge-list" aria-label="已确认知识">
      {items.map((item) => (
        <li key={item.id}>
          <button className="knowledge-item" onClick={() => onOpen(item.id)}>
            <strong>{item.title}</strong>
            <span className="detail-muted">v{item.currentVersionSeq} · {item.eligible ? '可复用' : '已退出检索'}</span>
          </button>
        </li>
      ))}
    </ul>
  )
}

export function CandidatesList({ onOpen }: { onOpen: (candidateId: string) => void }) {
  const [items, setItems] = useState<CandidateSummary[] | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    let cancelled = false
    void api.listCandidates()
      .then((page) => { if (!cancelled) setItems(page.items) })
      .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '候选暂时不可读。') })
    return () => { cancelled = true }
  }, [])
  if (error) return <p className="field-error" role="alert">{error}</p>
  if (!items) return <p className="inline-status">正在读取候选…</p>
  if (items.length === 0) {
    return <p className="detail-muted">还没有知识候选。从诊断结论的“整理为知识”开始。</p>
  }
  return (
    <ul className="knowledge-list" aria-label="知识候选">
      {items.map((item) => (
        <li key={item.id}>
          <button className="knowledge-item" onClick={() => onOpen(item.id)}>
            <strong>{item.draftTitle || '未命名草稿'}</strong>
            <span className="detail-muted">{candidateSourceLabels[item.sourceType]} · {candidateStateLabels[item.state]} · r{item.draftRevision}</span>
          </button>
        </li>
      ))}
    </ul>
  )
}

export function KnowledgeDetailPane({ knowledgeId }: { knowledgeId: string }) {
  const [detail, setDetail] = useState<KnowledgeDetail | null>(null)
  const [versions, setVersions] = useState<KnowledgeVersionSummary[]>([])
  const [scope, setScope] = useState<Record<string, unknown> | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    let cancelled = false
    // Deferred so the knowledge switch never sets state synchronously
    // inside the effect body (react-hooks/set-state-in-effect).
    queueMicrotask(() => {
      if (cancelled) return
      setError('')
      setDetail(null)
      setScope(null)
      void Promise.all([api.getKnowledge(knowledgeId), api.listVersions(knowledgeId)])
        .then(([next, page]) => {
          if (cancelled) return
          setDetail(next)
          setVersions(page.items)
          // The current version's scope rides along for the 适用范围 card.
          return api.getVersion(knowledgeId, next.currentVersionId)
        })
        .then((version) => { if (!cancelled && version) setScope(version.scope ?? null) })
        .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '知识暂时不可读。') })
    })
    return () => { cancelled = true }
  }, [knowledgeId])
  if (error) return <div className="detail-content"><p className="field-error" role="alert">{error}</p></div>
  if (!detail) return <p className="inline-status">正在读取知识…</p>
  return (
    <article className="knowledge-detail" aria-label="知识详情">
      <header className="detail-header">
        <p className="eyebrow">知识 · v{detail.currentVersionSeq}</p>
        <h1>{detail.title}</h1>
        <p className="detail-muted">
          共 {detail.versionCount} 个版本 · {detail.eligible ? '当前可被检索复用' : '已退出检索'}
        </p>
      </header>
      <section aria-label="适用范围">
        <h3>适用范围</h3>
        {scope && Object.keys(scope).length > 0
          ? <dl className="knowledge-scope">{Object.entries(scope).map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{String(value)}</dd></div>)}</dl>
          : <p className="detail-muted">未限定范围：适用于所有场景。</p>}
      </section>
      <section aria-label="版本历史">
        <h3>不可变版本历史</h3>
        <ol className="knowledge-versions">
          {versions.map((version) => (
            <li key={version.id} className={version.id === detail.currentVersionId ? 'current' : ''}>
              <strong>v{version.versionSeq} · {version.title}</strong>
              <span className="detail-muted">
                {version.id === detail.currentVersionId ? '当前版本 · ' : ''}
                {version.eligible ? '可检索' : '已退出检索'} · 创建于 {formatTime(version.createdAt)}
              </span>
            </li>
          ))}
        </ol>
      </section>
    </article>
  )
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}
