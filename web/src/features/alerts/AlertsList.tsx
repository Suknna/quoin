import { useEffect, useId, useRef, useState } from 'react'
import { useStreamPhase } from '../../app/realtime/hooks'
import { fetchBusinessSystems, type AlertOccurrenceSummary, type BusinessSystemOption } from './api'
import { useLiveAlerts } from './useLiveAlerts'

// The alerts rail (UI-ALERT-001/002/003): the current/history segment lives
// in the Workbench; this list shows the two-line rows with the attributed
// business system (未归属 when no system matches) and the searchable
// business-system filter the server actually supports.

interface AlertsProps {
  view: 'Firing' | 'Resolved'
  businessSystemKey: string
  onFilter: (key: string) => void
  onSelect: (id: string) => void
}

export function AlertsList({ view, businessSystemKey, onFilter, onSelect }: AlertsProps) {
  const live = useLiveAlerts(view, businessSystemKey)
  const phase = useStreamPhase()
  const [systems, setSystems] = useState<BusinessSystemOption[]>([])
  const [systemsFailed, setSystemsFailed] = useState(false)
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(-1)
  const boxRef = useRef<HTMLDivElement>(null)
  const listboxID = useId()

  useEffect(() => {
    const onScroll = () => live.setAtTop(window.scrollY < 80)
    onScroll()
    window.addEventListener('scroll', onScroll, { passive: true })
    return () => window.removeEventListener('scroll', onScroll)
  }, [live])

  useEffect(() => {
    let cancelled = false
    void fetchBusinessSystems()
      .then((items) => {
        if (!cancelled) setSystems(items)
      })
      .catch(() => {
        if (!cancelled) setSystemsFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    const onClickOutside = (event: MouseEvent) => {
      if (boxRef.current && !boxRef.current.contains(event.target as Node)) {
        setOpen(false)
        setActiveIndex(-1)
      }
    }
    document.addEventListener('mousedown', onClickOutside)
    return () => document.removeEventListener('mousedown', onClickOutside)
  }, [])

  const selectedSystem = systems.find((system) => system.key === businessSystemKey) ?? null
  const normalizedQuery = query.toLowerCase()
  const matches = query
    ? systems.filter((system) => system.key.toLowerCase().includes(normalizedQuery) || system.displayName.toLowerCase().includes(normalizedQuery))
    : systems
  const options: BusinessSystemOption[] = [{ key: '', displayName: '全部业务系统' }, ...matches]

  function chooseSystem(key: string) {
    onFilter(key)
    setOpen(false)
    setQuery('')
    setActiveIndex(-1)
  }

  function handleComboboxKeyDown(event: React.KeyboardEvent<HTMLInputElement>) {
    if (event.key === 'Escape') {
      setOpen(false)
      setActiveIndex(-1)
      return
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault()
      setOpen(true)
      if (options.length === 0) return
      const step = event.key === 'ArrowDown' ? 1 : -1
      setActiveIndex((previous) => {
        if (previous === -1) return step === 1 ? 0 : options.length - 1
        return Math.max(0, Math.min(options.length - 1, previous + step))
      })
      return
    }
    if (event.key === 'Enter' && open && activeIndex >= 0 && options[activeIndex]) {
      event.preventDefault()
      chooseSystem(options[activeIndex].key)
    }
  }

  if (live.error) {
    return (
      <div className="inline-status" role="alert">
        <div><strong>告警列表暂时不可用</strong><p>{live.error}</p></div>
      </div>
    )
  }
  return (
    <div className="live-list" aria-busy={phase === 'recovering' || undefined}>
      <div className="filter-bar" ref={boxRef}>
        <div className="system-combobox">
          <input
            type="text"
            role="combobox"
            aria-label="按业务系统筛选"
            aria-autocomplete="list"
            aria-controls={listboxID}
            aria-expanded={open}
            aria-activedescendant={activeIndex >= 0 ? `${listboxID}-option-${activeIndex}` : undefined}
            placeholder="全部业务系统"
            value={open ? query : (selectedSystem?.displayName ?? businessSystemKey)}
            onFocus={() => {
              setOpen(true)
              setQuery('')
              setActiveIndex(-1)
            }}
            onChange={(event) => {
              setQuery(event.target.value)
              setOpen(true)
              setActiveIndex(-1)
            }}
            onKeyDown={handleComboboxKeyDown}
          />
          {open && (
            <ul id={listboxID} className="combobox-list" role="listbox" aria-label="业务系统">
              {options.map((system, index) => (
                <li
                  key={system.key || 'all'}
                  id={`${listboxID}-option-${index}`}
                  role="option"
                  aria-selected={activeIndex === index}
                  className={activeIndex === index ? 'active' : undefined}
                  onMouseDown={(event) => {
                    event.preventDefault()
                    chooseSystem(system.key)
                  }}
                >
                  {system.key ? `${system.displayName}（${system.key}）` : system.displayName}
                </li>
              ))}
            </ul>
          )}
          {systemsFailed && <p className="field-error">业务系统列表暂不可用</p>}
        </div>
        {businessSystemKey !== '' && (
          <button className="text-button" onClick={() => onFilter('')}>
            清除筛选
          </button>
        )}
      </div>
      {live.loading && live.items.length === 0 && (
        <div className="inline-status"><div><strong>正在读取告警…</strong></div></div>
      )}
      {!live.loading && live.items.length === 0 && live.pendingNew === 0 && (
        <div className="inline-status">
          <span className="status-dot waiting" />
          <div>
            <strong>{view === 'Firing' ? '当前没有 Firing 告警' : '还没有已恢复的告警'}</strong>
            <p>
              {businessSystemKey
                ? '该业务系统当前没有匹配的告警；可清除筛选查看全部。'
                : view === 'Firing'
                  ? 'Alertmanager 送达的新告警会实时出现在这里。'
                  : '告警恢复后会进入历史视图。'}
            </p>
          </div>
        </div>
      )}
      {live.pendingNew > 0 && (
        <button className="new-content-pill" onClick={live.mergePending}>
          有 {live.pendingNew} 条新告警，点击查看
        </button>
      )}
      <ul className="object-list-items">
        {live.items.map((occurrence) => (
          <AlertRow key={occurrence.id} occurrence={occurrence} onSelect={onSelect} />
        ))}
      </ul>
    </div>
  )
}

function AlertRow({ occurrence, onSelect }: { occurrence: AlertOccurrenceSummary; onSelect: (id: string) => void }) {
  const alertname = occurrence.labels['alertname'] ?? '(无名称)'
  const severity = occurrence.labels['severity']
  const time = formatTime(occurrence.lastStateChangeAt)
  return (
    <li>
      <button className="object-row" onClick={() => onSelect(occurrence.id)}>
        <span className={`alert-state ${occurrence.state === 'Firing' ? 'firing' : 'resolved'}`} aria-hidden="true">{occurrence.state === 'Firing' ? '●' : '○'}</span>
        <span className="alert-state-text">{occurrence.state === 'Firing' ? 'Firing' : '已恢复'}</span>
        <span className="object-row-main">
          <strong>{alertname}</strong>
          <span className="object-row-meta">
            <em>{occurrence.businessSystemKey ?? '未归属'}</em>
            {severity ? <em>{severity}</em> : null}
            <time dateTime={occurrence.lastStateChangeAt}>{time}</time>
          </span>
        </span>
      </button>
    </li>
  )
}

function formatTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}
