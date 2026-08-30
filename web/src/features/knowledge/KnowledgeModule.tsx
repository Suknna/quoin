import { useEffect, useRef, useState } from 'react'
import { feedbackValueLabels, fetchFeedback, type FeedbackTimeline, type FeedbackTargetType } from '../feedback/api'
import {
  api, candidateSourceLabels, candidateStateLabels,
  type CandidateSummary, type KnowledgeDetail, type KnowledgeQueryResult, type KnowledgeSearchHit, type KnowledgeSummary, type KnowledgeVersionDetail, type KnowledgeVersionSummary,
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
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [loadingMore, setLoadingMore] = useState(false)
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<KnowledgeQueryResult | null>(null)
  const [searchCursor, setSearchCursor] = useState<string | undefined>()
  // The cursor is only valid for the query that minted it: load-more always
  // uses the bound query, never the (editable) input box content.
  const activeSearchQueryRef = useRef('')
  const searchGenerationRef = useRef(0)
  const [error, setError] = useState('')
  useEffect(() => {
    let cancelled = false
    void api.browse()
      .then((page) => { if (!cancelled) { setItems(page.items); setNextCursor(page.nextCursor) } })
      .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '知识暂时不可读。') })
    return () => { cancelled = true }
  }, [])
  async function loadMoreBrowse() {
    if (!nextCursor) return
    setLoadingMore(true)
    try { const page = await api.browse(nextCursor); setItems((current) => [...(current ?? []), ...page.items]); setNextCursor(page.nextCursor) } catch (reason) { setError(reason instanceof Error ? reason.message : '暂时无法读取更多知识。') } finally { setLoadingMore(false) }
  }
  async function search() {
    const text = query.trim()
    // Every submit invalidates all in-flight responses — including the empty
    // reset, which must never be resurrected by a late arrival.
    const generation = ++searchGenerationRef.current
    if (!text) { setResults(null); setSearchCursor(undefined); activeSearchQueryRef.current = ''; return }
    setError('')
    activeSearchQueryRef.current = text
    try {
      const page = await api.search(text)
      if (generation !== searchGenerationRef.current) return
      setResults(page); setSearchCursor(page.nextCursor)
    } catch (reason) {
      if (generation !== searchGenerationRef.current) return
      setError(reason instanceof Error ? reason.message : '暂时无法检索知识。')
    }
  }
  async function loadMoreSearch() {
    if (!results || !searchCursor) return
    setLoadingMore(true)
    const generation = searchGenerationRef.current
    try {
      const page = await api.search(activeSearchQueryRef.current, searchCursor)
      if (generation !== searchGenerationRef.current) return
      setResults({
        mode: 'query',
        exactTextMatches: [...results.exactTextMatches, ...page.exactTextMatches],
        semanticMatches: [...results.semanticMatches, ...page.semanticMatches],
        nextCursor: page.nextCursor,
      })
      setSearchCursor(page.nextCursor)
    } catch (reason) {
      if (generation !== searchGenerationRef.current) return
      setError(reason instanceof Error ? reason.message : '暂时无法读取更多检索结果。')
    } finally { setLoadingMore(false) }
  }
  const renderItems = (matches: KnowledgeSummary[], label: string) => <section aria-label={label}><h3>{label}</h3>{matches.length === 0 ? <p className="detail-muted">没有匹配项。</p> : <ul className="knowledge-list">{matches.map((item) => <li key={item.id}><button className="knowledge-item" onClick={() => onOpen(item.id)}><strong>{item.title}</strong><span className="detail-muted">v{item.currentVersionSeq} · {item.eligible ? '可复用' : '已退出检索'}</span></button></li>)}</ul>}</section>
  const renderHits = (matches: KnowledgeSearchHit[], label: string, secondaryScores: Map<string, number>) => <section aria-label={label}><h3>{label}</h3>{matches.length === 0 ? <p className="detail-muted">没有匹配项。</p> : <ul className="knowledge-list">{matches.map((hit) => <li key={hit.knowledge.id}><button className="knowledge-item" onClick={() => onOpen(hit.knowledge.id)}><strong>{hit.knowledge.title}</strong><span className="detail-muted">{label}分数 {hit.score.toFixed(3)}{secondaryScores.has(hit.knowledge.id) ? ` · 同时命中另一种检索（分数 ${secondaryScores.get(hit.knowledge.id)?.toFixed(3)}）` : ''}</span></button></li>)}</ul>}</section>
  if (error) return <p className="field-error" role="alert">{error}</p>
  if (!items) return <p className="inline-status">正在读取知识…</p>
  const exact = results?.exactTextMatches ?? []
  const exactIDs = new Set(exact.map((hit) => hit.knowledge.id))
  const semantic = (results?.semanticMatches ?? []).filter((hit) => !exactIDs.has(hit.knowledge.id))
  const semanticScores = new Map((results?.semanticMatches ?? []).map((hit) => [hit.knowledge.id, hit.score]))
  const exactScores = new Map(exact.map((hit) => [hit.knowledge.id, hit.score]))
  return <>
    <form className="knowledge-search" onSubmit={(event) => { event.preventDefault(); void search() }}><label className="field"><span>检索知识</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="输入关键词" /></label><button className="secondary-button" type="submit">搜索</button></form>
    {results ? <>{renderHits(exact, '精确文本匹配', semanticScores)}{renderHits(semantic, '语义相似', exactScores)}{searchCursor && <button className="secondary-button" disabled={loadingMore} onClick={() => void loadMoreSearch()}>{loadingMore ? '正在加载…' : '加载更多'}</button>}</> : <>{items.length === 0 ? <p className="detail-muted">还没有确认过任何知识。在初步分析、巡检报告或调查消息上使用“整理为知识”，确认后会出现在这里。</p> : renderItems(items, '已确认知识')}{nextCursor && <button className="secondary-button" disabled={loadingMore} onClick={() => void loadMoreBrowse()}>{loadingMore ? '正在加载…' : '加载更多'}</button>}</>}
  </>
}

export function CandidatesList({ onOpen }: { onOpen: (candidateId: string) => void }) {
  const [items, setItems] = useState<CandidateSummary[] | null>(null)
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    let cancelled = false
    void api.listCandidates()
      .then((page) => { if (!cancelled) { setItems(page.items); setNextCursor(page.nextCursor) } })
      .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '候选暂时不可读。') })
    return () => { cancelled = true }
  }, [])
  async function loadMore() {
    if (!nextCursor) return
    setLoadingMore(true)
    try { const page = await api.listCandidates('', nextCursor); setItems((current) => [...(current ?? []), ...page.items]); setNextCursor(page.nextCursor) } catch (reason) { setError(reason instanceof Error ? reason.message : '暂时无法读取更多候选。') } finally { setLoadingMore(false) }
  }
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
      {nextCursor && <li><button className="secondary-button" disabled={loadingMore} onClick={() => void loadMore()}>{loadingMore ? '正在加载…' : '加载更多'}</button></li>}
    </ul>
  )
}

export function KnowledgeDetailPane({ knowledgeId }: { knowledgeId: string }) {
  const [detail, setDetail] = useState<KnowledgeDetail | null>(null)
  const [versions, setVersions] = useState<KnowledgeVersionSummary[]>([])
  const [versionCursor, setVersionCursor] = useState<string | undefined>()
  const [loadingVersions, setLoadingVersions] = useState(false)
  const [scope, setScope] = useState<Record<string, unknown> | null>(null)
  const [currentVersion, setCurrentVersion] = useState<KnowledgeVersionDetail | null>(null)
  const [sourceCandidate, setSourceCandidate] = useState<CandidateSummary | null>(null)
  const [feedback, setFeedback] = useState<FeedbackTimeline | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirmingStop, setConfirmingStop] = useState(false)
  useEffect(() => {
    let cancelled = false
    // Deferred so the knowledge switch never sets state synchronously
    // inside the effect body (react-hooks/set-state-in-effect).
    queueMicrotask(() => {
      if (cancelled) return
      setError('')
      setDetail(null)
      setScope(null)
      setCurrentVersion(null)
      setSourceCandidate(null)
      setFeedback(null)
      void Promise.all([api.getKnowledge(knowledgeId), api.listVersions(knowledgeId)])
        .then(([next, page]) => {
          if (cancelled) return
          setDetail(next)
          setVersions(page.items)
          setVersionCursor(page.nextCursor)
          // The current version's scope rides along for the 适用范围 card.
          return api.getVersion(knowledgeId, next.currentVersionId)
        })
        .then(async (version) => {
          if (cancelled || !version) return
          setScope(version.scope ?? null)
          setCurrentVersion(version)
          const candidate = await api.getCandidate(version.sourceCandidateId)
          if (cancelled) return
          setSourceCandidate(candidate)
          if (candidate.sourceType === 'initial_analysis_output' || candidate.sourceType === 'inspection_report' || candidate.sourceType === 'investigation_message') {
            const timeline = await fetchFeedback({ type: candidate.sourceType as FeedbackTargetType, id: candidate.sourceId })
            if (!cancelled) setFeedback(timeline)
          }
        })
        .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '知识暂时不可读。') })
    })
    return () => { cancelled = true }
  }, [knowledgeId])
  async function loadMoreVersions() {
    if (!versionCursor) return
    setLoadingVersions(true)
    try { const page = await api.listVersions(detail!.id, versionCursor); setVersions((current) => [...current, ...page.items]); setVersionCursor(page.nextCursor) } finally { setLoadingVersions(false) }
  }
  if (error) return <div className="detail-content"><p className="field-error" role="alert">{error}</p></div>
  if (!detail) return <p className="inline-status">正在读取知识…</p>
  const currentDetail = detail
  async function revise() {
    setBusy(true); setError('')
    try { const candidate = await api.createRevision(currentDetail.id, currentDetail.currentVersionId, currentDetail.rowVersion); window.history.pushState({}, '', `/knowledge/candidates/${candidate.id}`); window.dispatchEvent(new PopStateEvent('popstate')) } catch (reason) { setError(reason instanceof Error ? reason.message : '暂时无法创建修订草稿。') } finally { setBusy(false) }
  }
  async function stopReuse() {
    setBusy(true); setError('')
    try {
      await api.stopReuse(currentDetail.id, currentDetail.currentVersionId, versions.find((version) => version.id === currentDetail.currentVersionId)?.retrievalStateRowVersion ?? 1)
      const [nextDetail, nextVersions, nextCurrent] = await Promise.all([
        api.getKnowledge(currentDetail.id), api.listVersions(currentDetail.id), api.getVersion(currentDetail.id, currentDetail.currentVersionId),
      ])
      setDetail(nextDetail); setVersions(nextVersions.items); setCurrentVersion(nextCurrent); setVersionCursor(nextVersions.nextCursor); setConfirmingStop(false)
    } catch (reason) { setError(reason instanceof Error ? reason.message : '暂时无法停止复用。') } finally { setBusy(false) }
  }
  return (
    <article className="knowledge-detail" aria-label="知识详情">
      <header className="detail-header">
        <p className="eyebrow">知识 · v{detail.currentVersionSeq}</p>
        <h1>{detail.title}</h1>
        <p className="detail-muted">
          共 {detail.versionCount} 个版本 · {detail.eligible ? '当前可被检索复用' : '已退出检索'}
        </p>
        <div className="candidate-actions"><button className="secondary-button" disabled={busy} onClick={() => void revise()}>创建修订草稿</button>{detail.eligible && <button className="text-button" disabled={busy} onClick={() => setConfirmingStop(true)}>停止复用</button>}</div>
      </header>
      {confirmingStop && <div className="dialog-scrim" role="presentation" onClick={() => setConfirmingStop(false)}><div className="dialog-panel" role="dialog" aria-modal="true" aria-labelledby="stop-reuse-title" onClick={(event) => event.stopPropagation()} onKeyDown={(event) => { if (event.key === 'Escape') setConfirmingStop(false) }}><h3 id="stop-reuse-title">停止复用当前版本？</h3><p>该版本会永久退出检索，不能重新启用。若要恢复内容，请创建并确认一个新版本。</p><div className="dialog-actions"><button className="secondary-button" autoFocus onClick={() => setConfirmingStop(false)}>取消</button><button className="primary-button danger" disabled={busy} onClick={() => void stopReuse()}>确认停止复用</button></div></div></div>}
      <section aria-label="当前版本">
        <h3>当前版本</h3>
        {currentVersion ? <><p>{currentVersion.body}</p><p className="detail-muted">检索资格：{currentVersion.eligible ? '可检索' : '已退出检索'} · Embedding 索引：{currentVersion.embeddingState}</p></> : <p className="detail-muted">正在读取当前版本…</p>}
      </section>
      <section aria-label="来源诊断与反馈">
        <h3>来源诊断与反馈</h3>
        {sourceCandidate ? <p className="detail-muted">来源：{candidateSourceLabels[sourceCandidate.sourceType]}（来源记录 {sourceCandidate.sourceId}）</p> : <p className="detail-muted">正在读取来源…</p>}
        {feedback ? <p className="detail-muted">最新反馈：{feedback.latestValue ? feedbackValueLabels[feedback.latestValue] : '尚未记录'} · 共 {feedback.items.length} 条</p> : sourceCandidate && <p className="detail-muted">该来源尚无可展示的反馈。</p>}
      </section>
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
        {versionCursor && <button className="secondary-button" disabled={loadingVersions} onClick={() => void loadMoreVersions()}>{loadingVersions ? '正在加载…' : '加载更早版本'}</button>}
      </section>
    </article>
  )
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}
