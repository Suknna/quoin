import { useEffect, useRef, useState } from 'react'
import type { InitialAnalysisDetail, InitialAnalysisSummary } from './api'
import { analysisCommandId, cancelAnalysis, createAnalysis, fetchAnalyses, fetchAnalysis, fetchAttempts, isActive, reasonLabel, stateLabel } from './api'

// AnalysisPanel is the inline Initial Analysis section of the alert detail
// (UI-ALERT-004): one primary action, the real stage while running, the
// cancellation fence, and the immutable history with the latest result
// first. Terminal state stays visible after leaving and returning.
export function AnalysisPanel({ occurrenceId, onOpenAnalysis }: { occurrenceId: string; onOpenAnalysis: (analysis: InitialAnalysisDetail) => void }) {
  const [items, setItems] = useState<InitialAnalysisSummary[]>([])
  const [detail, setDetail] = useState<InitialAnalysisDetail | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [outcomeReason, setOutcomeReason] = useState('')
  const timer = useRef<number | null>(null)

	const load = () => {
		void fetchAnalyses(occurrenceId)
			.then((page) => setItems(page.items ?? []))
			.catch((reason: unknown) => setError(reason instanceof Error ? reason.message : '初步分析加载失败'))
	}
	useEffect(() => {
		// Switching occurrences resets the panel state (the last detail
		// belongs to the previous occurrence).
		setItems([])
		setDetail(null)
		load()
		return () => { if (timer.current !== null) window.clearTimeout(timer.current) }
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [occurrenceId])

  const active = items.find((item) => isActive(item.state))

  // A terminal technical outcome explains itself: the latest attempt's
  // termination reason reads back through the immutable attempt history
  // so 失败/中断 never render as bare state pills (UI-ALERT-004).
  useEffect(() => {
    if (!detail || detail.state === 'Succeeded') {
      setOutcomeReason('')
      return
    }
    if (isActive(detail.state)) return
    let cancelled = false
    void fetchAttempts(occurrenceId, detail.id)
      .then((page) => {
        if (cancelled) return
        const history = page.items ?? []
        const latest = history.length > 0 ? history[history.length - 1] : undefined // attempts read back oldest-first
        setOutcomeReason(reasonLabel(latest?.terminationReason))
      })
      .catch(() => undefined)
    return () => { cancelled = true }
  }, [occurrenceId, detail?.id, detail?.state]) // eslint-disable-line react-hooks/exhaustive-deps

	// The active analysis is polled for its authoritative state; the
	// task SSE is the transport-level live channel, polling re-reads the
	// authority (HTTP-SSE-007: only ids/versions travel on the stream).
	// When the analysis terminates the LAST detail stays visible (the
	// sealed output is the terminal record).
	useEffect(() => {
		if (!active) {
			return
		}
    const poll = () => {
      void fetchAnalysis(occurrenceId, active.id)
        .then((next) => {
          setDetail(next)
          setItems((current) => {
            const rest = current.filter((item) => item.id !== active.id)
            return [next, ...rest].sort((a, b) => Number(b.id) - Number(a.id))
          })
        })
        .catch(() => undefined)
    }
    poll()
    timer.current = window.setInterval(poll, 2000)
    return () => { if (timer.current !== null) window.clearInterval(timer.current) }
    // The poller re-reads the authority of the active analysis; `active`
    // identity change re-arms it via the dependency below.
  }, [occurrenceId, active?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  async function start() {
    setBusy(true)
    setError('')
    try {
      const created = await createAnalysis(occurrenceId, analysisCommandId())
      setDetail(created)
      load()
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法发起初步分析')
    } finally {
      setBusy(false)
    }
  }

  async function cancel() {
    if (!detail) return
    // The button flips to a non-repeatable "正在停止" until the server
    // confirms the fence (工作台投影: Stop/Cancel 提交后立即变为不可重复触发).
    setStopping(true)
    setError('')
    try {
      const next = await cancelAnalysis(occurrenceId, detail.id, detail.rowVersion, analysisCommandId())
      setDetail(next)
      load()
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法取消初步分析')
    } finally {
      setStopping(false)
    }
  }

  return (
    <section className="detail-section analysis-section" aria-labelledby="analysis-title">
      <div className="analysis-heading">
        <h2 id="analysis-title">初步分析</h2>
        {!active && (
          <button className="primary-button compact" onClick={start} disabled={busy} aria-busy={busy}>
            {busy ? '正在受理…' : '初步分析'}
          </button>
        )}
        {active && (
          <span className="analysis-live" role="status">
            <span className="status-dot running" />
            {detail ? stateLabel(detail.state) : stateLabel(active.state)}
          </span>
        )}
      </div>
      {error && <div className="error-summary" role="alert"><strong>操作未完成</strong><span>{error}</span></div>}
      {active && (
        <div className="analysis-progress">
          <p>
            当前 {stateLabel(active.state)}
            {detail && detail.attemptCount > 1 ? ` · 第 ${detail.attemptCount} 次 Attempt` : ''}
          </p>
          <button className="text-button" onClick={cancel} disabled={stopping} aria-busy={stopping}>
            {stopping ? '正在停止…' : '取消'}
          </button>
        </div>
      )}
      {detail?.output && (
        <div className="analysis-latest">
          <strong>最新结论</strong>
          <p className="analysis-preview">{detail.output.content.slice(0, 240)}{detail.output.content.length > 240 ? '…' : ''}</p>
          <button className="text-button" onClick={() => onOpenAnalysis(detail)}>查看完整分析</button>
        </div>
      )}
      {detail && !isActive(detail.state) && outcomeReason && (
        <p className="analysis-outcome" role="note">{stateLabel(detail.state)} · {outcomeReason}。可重新发起初步分析。</p>
      )}
      <ol className="analysis-history" aria-label="分析历史">
        {items.filter((item) => !isActive(item.state)).map((item) => (
          <li key={item.id}>
            <button className="analysis-history-item" onClick={() => { void fetchAnalysis(occurrenceId, item.id).then(onOpenAnalysis).catch(() => undefined) }}>
              <span className={`status-pill ${item.state.toLowerCase()}`}>{stateLabel(item.state)}</span>
              <time dateTime={item.createdAt}>{formatTime(item.createdAt)}</time>
            </button>
          </li>
        ))}
      </ol>
      {items.length === 0 && !active && <p className="detail-muted">尚未发起过初步分析。</p>}
    </section>
  )
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}
