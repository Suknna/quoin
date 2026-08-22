import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import {
  ConfigApiError,
  activateLabelContract,
  fetchLabelContractReadiness,
  formatTime,
  listLabelContracts,
  readinessBlockerText,
  type ActivationItemInput,
  type LabelContractReadiness,
  type LabelContractSummary,
} from './api'

// Label Contract readiness + atomic activation layer (T17, CONTEXT
//「Label Contract 激活投影」): one full-workbench view showing the target
// contract, every enabled system's legal candidate pairs and blockers; the
// Admin explicitly selects one candidate per system, then a single confirm
// activates everything atomically. Blocked systems deep-link to the exact
// version detail. No partial activation — a 409 re-reads the readiness view.

interface LabelContractsPanelProps {
  isAdmin: boolean
  onClose: () => void
  onOpenSystem: (systemKey: string) => void
}

export function LabelContractsPanel({ isAdmin, onClose, onOpenSystem }: LabelContractsPanelProps) {
  const [contracts, setContracts] = useState<LabelContractSummary[]>([])
  const [selected, setSelected] = useState<number | null>(null)
  const [readiness, setReadiness] = useState<LabelContractReadiness | null>(null)
  const [choices, setChoices] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [loadError, setLoadError] = useState('')
  const [confirming, setConfirming] = useState(false)
  const [activating, setActivating] = useState(false)
  const [actionError, setActionError] = useState('')
  const panelRef = useRef<HTMLDivElement>(null)
  const closeButtonRef = useRef<HTMLButtonElement>(null)
  const activationTriggerRef = useRef<HTMLButtonElement>(null)
  const confirmationRef = useRef<HTMLDivElement>(null)
  const confirmationCancelRef = useRef<HTMLButtonElement>(null)
  const openerRef = useRef<HTMLElement | null>(null)
  const selectedVersionRef = useRef<number | null>(selected)
  const readinessRequestRef = useRef(0)
  selectedVersionRef.current = selected

  useEffect(() => {
    let cancelled = false
    void listLabelContracts()
      .then((items) => {
        if (cancelled) return
        setContracts(items)
        const draft = items.find((item) => item.state === 'draft') ?? null
        setSelected(draft ? draft.version : null)
      })
      .catch((reason: unknown) => {
        if (!cancelled) setLoadError(reason instanceof Error ? reason.message : '暂时无法读取契约列表。')
      })
    return () => {
      cancelled = true
    }
  }, [])

  const loadReadiness = useCallback((version: number) => {
    // A conflict retry can outlive a contract selection change. Never let it
    // clear or replace the new target's readiness projection.
    if (selectedVersionRef.current !== version) return () => undefined
    const request = ++readinessRequestRef.current
    const controller = new AbortController()
    setReadiness(null)
    setError('')
    void fetchLabelContractReadiness(version, controller.signal)
      .then((view) => {
        if (controller.signal.aborted || request !== readinessRequestRef.current || selectedVersionRef.current !== version || view.targetContractVersion !== version) return
        setReadiness(view)
        // An explicit choice is required per system whenever more than one
        // candidate exists; the single-candidate case is preselected because
        // it is the only legal option (never "latest wins").
        const next: Record<string, string> = {}
        for (const system of view.systems) {
          if (system.activationCandidates.length === 1) {
            next[system.businessSystemKey] = system.activationCandidates[0].passedVerificationRunId
          }
        }
        setChoices(next)
      })
      .catch((reason: unknown) => {
        if (controller.signal.aborted || request !== readinessRequestRef.current || selectedVersionRef.current !== version) return
        setError(reason instanceof Error ? reason.message : '暂时无法读取就绪视图。')
      })
    return () => controller.abort()
  }, [])

  useEffect(() => {
    if (selected === null) return
    return loadReadiness(selected)
  }, [selected, loadReadiness])

  useEffect(() => {
    openerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    closeButtonRef.current?.focus()
    return () => {
      if (openerRef.current?.isConnected) openerRef.current.focus()
    }
  }, [])

  useEffect(() => {
    if (confirming) confirmationCancelRef.current?.focus()
  }, [confirming])

  function dismissConfirmation() {
    setConfirming(false)
    requestAnimationFrame(() => activationTriggerRef.current?.focus())
  }

  function handlePanelKeyDown(event: ReactKeyboardEvent<HTMLDivElement>) {
    if (event.key === 'Escape') {
      if (confirming) {
        if (!activating) dismissConfirmation()
      } else {
        onClose()
      }
      return
    }
    if (event.key !== 'Tab') return
    const scope = confirming ? confirmationRef.current : panelRef.current
    if (!scope) return
    const focusable = Array.from(scope.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href]',
    ))
    if (focusable.length === 0) return
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
  }

  const selectedContract = contracts.find((item) => item.version === selected) ?? null
  const blocked = useMemo(
    () => (readiness?.systems ?? []).filter((system) => system.activationCandidates.length === 0),
    [readiness],
  )
  const selectable = useMemo(
    () => (readiness?.systems ?? []).filter((system) => system.activationCandidates.length > 0),
    [readiness],
  )
  // The first contract has no enabled systems. Its empty compatibility set is
  // deliberately a complete choice, matching the server's atomic command.
  const allChosen = selectable.every((system) => choices[system.businessSystemKey] !== undefined)
  const canActivate = isAdmin && selectedContract?.state === 'draft' && readiness !== null && blocked.length === 0 && allChosen

  async function activate() {
    if (!readiness || !selectedContract) return
    const targetVersion = selectedContract.version
    setActivating(true)
    setActionError('')
    try {
      const compatibleVersions: ActivationItemInput[] = selectable.map((system) => {
        const chosenVerificationRunID = choices[system.businessSystemKey]
        const candidate = system.activationCandidates.find((item) => item.passedVerificationRunId === chosenVerificationRunID)
        if (!candidate) throw new ConfigApiError('请为每个系统选择一个候选版本。', 422)
        return {
          businessSystemKey: system.businessSystemKey,
          configVersionId: candidate.configVersionId,
          verificationRunId: candidate.passedVerificationRunId,
          expectedCurrentConfigVersionId: system.currentConfigVersionId ?? null,
          expectedBusinessSystemRowVersion: system.businessSystemRowVersion,
        }
      })
      await activateLabelContract(targetVersion, {
        expectedStateRowVersion: readiness.stateRowVersion,
        expectedCurrentContractVersionId: readiness.currentContractVersionId ?? null,
        expectedTargetRowVersion: readiness.targetRowVersion,
        compatibleVersions,
      })
      onClose()
    } catch (reason) {
      setActionError(reason instanceof Error ? reason.message : '激活失败，请重试。')
      dismissConfirmation()
      // A deterministic conflict means the readiness preconditions moved:
      // re-read the authoritative view instead of resubmitting stale values.
      loadReadiness(targetVersion)
    } finally {
      setActivating(false)
    }
  }

  return (
    <div ref={panelRef} className="reading-overlay" role="dialog" aria-modal="true" aria-labelledby="label-contract-panel-title" onKeyDown={handlePanelKeyDown}>
      <div className="detail-content label-contract-panel">
        <div inert={confirming || undefined} aria-hidden={confirming || undefined}>
          <button ref={closeButtonRef} className="secondary-button compact" onClick={onClose}>
          关闭
        </button>
        <header className="detail-header">
          <p className="eyebrow">配置</p>
          <h1 id="label-contract-panel-title">Label Contract 与激活就绪</h1>
        </header>
        {loadError && (
          <div className="error-summary" role="alert">
            <strong>暂时无法读取契约列表</strong>
            <span>{loadError}</span>
          </div>
        )}
        {!loadError && contracts.length === 0 && (
          <div className="inline-status">
            <div>
              <strong>还没有 Label Contract</strong>
              <p>上传业务系统配置前，先通过“上传配置”入口创建第一个契约。</p>
            </div>
          </div>
        )}
        {contracts.length > 0 && (
          <section aria-labelledby="lc-select-title">
            <h2 id="lc-select-title">目标契约</h2>
            <div className="contract-picker" role="radiogroup" aria-label="选择目标契约">
              {contracts.map((contract) => (
                <label key={contract.version} className={`contract-option ${selected === contract.version ? 'selected' : ''}`}>
                  <input
                    type="radio"
                    name="target-contract"
                    value={contract.version}
                    checked={selected === contract.version}
                    onChange={() => setSelected(contract.version)}
                  />
                  <span>
                    v{contract.version} · {contract.state === 'draft' ? '草稿' : contract.state === 'active' ? '当前激活' : '已退役'}
                    <time dateTime={contract.createdAt}> {formatTime(contract.createdAt)}</time>
                  </span>
                </label>
              ))}
            </div>
            {selectedContract?.state !== 'draft' && selectedContract && (
              <p className="detail-muted">只有草稿契约可以被激活；当前视图仍展示其就绪状态。</p>
            )}
          </section>
        )}
        {error && (
          <div className="error-summary" role="alert">
            <strong>暂时无法读取就绪视图</strong>
            <span>{error}</span>
          </div>
        )}
        {readiness && (
          <section aria-labelledby="lc-readiness-title">
            <h2 id="lc-readiness-title">逐系统就绪</h2>
            {readiness.systems.length === 0 && (
              <p className="detail-muted">当前没有启用中的业务系统；可以直接原子激活该契约。</p>
            )}
            <ul className="readiness-list">
              {readiness.systems.map((system) => (
                <li key={system.businessSystemKey} className="readiness-system">
                  <div className="readiness-head">
                    <strong>{system.businessSystemKey}</strong>
                    <span className="detail-muted">
                      当前配置 {system.currentConfigVersionId ? `#${system.currentConfigVersionId}` : '（未发布）'}
                    </span>
                  </div>
                  {system.activationCandidates.length > 0 ? (
                    <div className="candidate-set" role="radiogroup" aria-label={`${system.businessSystemKey} 的候选版本`}>
                      {system.activationCandidates.map((candidate) => (
                        <label key={candidate.configVersionId + candidate.passedVerificationRunId} className="candidate-option">
                          <input
                            type="radio"
                            name={`candidate-${system.businessSystemKey}`}
                            value={candidate.passedVerificationRunId}
                            checked={choices[system.businessSystemKey] === candidate.passedVerificationRunId}
                            onChange={() =>
                              setChoices((previous) => ({ ...previous, [system.businessSystemKey]: candidate.passedVerificationRunId }))
                            }
                            disabled={!isAdmin}
                          />
                          <span>
                            配置版本 #{candidate.configVersionId} · 验证 Run #{candidate.passedVerificationRunId}（已通过）
                          </span>
                        </label>
                      ))}
                    </div>
                  ) : (
                    <div className="blocker-box">
                      <p className="field-error">
                        {system.blockers.map((blocker) => readinessBlockerText[blocker]).join('；')}
                      </p>
                      <button className="secondary-button compact" onClick={() => onOpenSystem(system.businessSystemKey)}>
                        前往业务系统
                      </button>
                    </div>
                  )}
                </li>
              ))}
            </ul>
            {isAdmin && selectedContract?.state === 'draft' && (
              <div className="publish-block">
                {!confirming && (
                  <button
                    ref={activationTriggerRef}
                    className="primary-button"
                    disabled={!canActivate}
                    onClick={() => setConfirming(true)}
                    title={canActivate ? undefined : '存在阻塞系统或未完成候选选择'}
                  >
                    原子激活此契约
                  </button>
                )}
                {actionError && (
                  <p className="field-error" role="alert">
                    {actionError}
                  </p>
                )}
              </div>
            )}
          </section>
        )}
        </div>
        {confirming && readiness && (
          <div className="confirm-layer">
            <div ref={confirmationRef} className="confirm-dialog" role="dialog" aria-modal="true" aria-label="确认原子激活">
              <p>
                将激活契约 v{readiness.targetContractVersion}
                ，并同时把以上各启用系统的当前配置切换到所选版本；任一前提失效则整体不生效。已开始的巡检继续使用旧版本，历史归属不会被改写。
              </p>
              <div className="dialog-actions">
                <button className="primary-button" disabled={!canActivate || activating} onClick={() => void activate()}>
                  {activating ? '正在激活…' : '确认原子激活'}
                </button>
                <button ref={confirmationCancelRef} className="secondary-button" disabled={activating} onClick={dismissConfirmation}>
                  取消
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
