import { useCallback, useEffect, useRef, useState } from 'react'
import type { UserSummary } from '../../../api/generated/types'
import {
  cancelDrainTarget,
  drainTargetOf,
  exitMaintenance,
  fetchMaintenanceState,
  prepareUpgrade,
  type MaintenanceStateView,
} from './api'

const stateLabels: Record<string, string> = {
  queued: '排队中',
  assigned: '已派发',
  running: '运行中',
  cancelling: '取消中',
  waitingforcapacity: '等待容量',
  starting: '启动中',
  awaitingreconnect: '等待重连',
}

const kindLabels: Record<string, string> = {
  ActiveAttempt: '活动任务',
  ActiveBrowserOperation: '浏览器操作',
  BackupPreflight: '升级前备份',
}

// The Upgrade maintenance checklist (UI-ADMIN-010, HTTP-MAINT-005): the
// server projection is the only authority. Admins drain remaining work with
// the frozen cancel commands, watch the reconciler verify the pre-upgrade
// backup, and may abort the upgrade while the wizard has not stopped the
// stack. There is no force/skip.
export function UpgradeMaintenancePanel({ user, initialState }: { user: UserSummary; initialState: MaintenanceStateView }) {
  const [state, setState] = useState(initialState)
  const [message, setMessage] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const pollTimer = useRef<number | undefined>(undefined)

  const refresh = useCallback(() => {
    void fetchMaintenanceState().then(setState, () => undefined)
  }, [])

  useEffect(() => {
    pollTimer.current = window.setInterval(refresh, 2_000)
    return () => window.clearInterval(pollTimer.current)
  }, [refresh])

  const prepared = state.items.length > 0 && state.items.every(item => item.safeState === 'Safe')

  const onDrain = async (objectKey: string) => {
    const item = state.items.find(candidate => candidate.objectKey === objectKey)
    if (!item) return
    const target = drainTargetOf(item)
    if (!target) return
    setBusy(true)
    setMessage(null)
    try {
      await cancelDrainTarget(target)
      setMessage('已提交取消，等待系统确认收口。')
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : '取消没有完成，清单刷新后会更新重试参数。')
    } finally {
      setBusy(false)
      refresh()
    }
  }

  const onExit = async () => {
    setBusy(true)
    setMessage(null)
    try {
      await exitMaintenance(state.rowVersion, 'Upgrade')
      // The application shell intentionally stops polling while maintenance
      // is active; the completed exit reloads into the restored workbench.
      window.location.reload()
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : '退出没有完成，请刷新后重试。')
      setBusy(false)
      refresh()
    }
  }

  return <section className="upgrade-drain" aria-label="升级维护清单">
    {prepared
      ? <p className="upgrade-prepared" role="status">升级准备完成：活动工作已清零，升级前备份已验证（quoin_upgrade_prepared=1）。部署向导可以继续停机升级；也可以在向导停机前取消本次升级。</p>
      : <p>系统已停止接收新工作。请取消或等待剩余活动工作结束；全部清零后系统会自动创建并验证升级前备份。</p>}
    <ul className="maintenance-checklist" role="list">
      {state.items.map(item => {
        const target = drainTargetOf(item)
        const [stateText] = item.detailCode.split('|')
        return <li key={`${item.kind}:${item.objectKey}`}>
          <span>{kindLabels[item.kind] ?? item.kind} · {stateLabels[stateText] ?? stateText}</span>
          {item.safeState === 'Blocking' && target
            ? <button className="text-button" disabled={busy} onClick={() => void onDrain(item.objectKey)}>取消</button>
            : null}
          <strong>{item.safeState === 'Safe' ? (item.kind === 'BackupPreflight' ? '备份已验证' : '已安全') : '需要处理'}</strong>
        </li>
      })}
    </ul>
    {message ? <p role="status">{message}</p> : null}
    {user.role === 'admin'
      ? <button className="primary-button" disabled={busy || !prepared} onClick={() => void onExit()}>取消升级并退出维护</button>
      : <p>请联系管理员完成升级维护。</p>}
  </section>
}

// PrepareUpgradeCard is the normal-mode Admin entry: the operator starts the
// coordinated upgrade here, then the surface switches to the maintenance
// checklist above.
export function PrepareUpgradeCard({ onPrepared }: { onPrepared: (state: MaintenanceStateView) => void }) {
  const [rowVersion, setRowVersion] = useState(1)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState<string | null>(null)

  useEffect(() => {
    void fetchMaintenanceState().then(state => setRowVersion(state.rowVersion), () => undefined)
  }, [])

  const onPrepare = async () => {
    setBusy(true)
    setMessage(null)
    try {
      const state = await prepareUpgrade(rowVersion)
      onPrepared(state)
      setMessage('已进入升级维护。')
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : '进入升级维护没有完成，请重试。')
      void fetchMaintenanceState().then(state => setRowVersion(state.rowVersion), () => undefined)
    } finally {
      setBusy(false)
    }
  }

  return <section className="prepare-upgrade" aria-label="协调升级">
    <h3>协调升级</h3>
    <p>进入升级维护会停止新任务与调度；剩余活动工作需要在维护清单中取消或等待结束，然后系统自动创建并验证升级前备份。部署向导以 quoin_upgrade_prepared 指标确认后再停机。</p>
    <button className="primary-button" disabled={busy} onClick={() => void onPrepare()}>准备升级维护</button>
    {message ? <p role="status">{message}</p> : null}
  </section>
}
