import { useEffect, useState } from 'react'
import {
  getJourneyCatalog,
  listLabelContracts,
  uploadBusinessSystemConfig,
  ConfigApiError,
  type FieldError,
  type JourneyCatalogView,
  type LabelContractSummary,
} from './api'

// The Admin upload layer (UI-SYSTEM-004): a full-workbench overlay with the
// template download, drag/drop or picker for exactly one YAML file, the
// target Label Contract and the embedded Journey Catalog provenance. Static
// validation failures render per-YAML-path reasons with remediation and the
// chosen file is retained for correction; there is no competing editor.

interface UploadOverlayProps {
  onClose: () => void
  onUploaded: (systemKey: string, versionId: string) => void
}

export function UploadOverlay({ onClose, onUploaded }: UploadOverlayProps) {
  const [contracts, setContracts] = useState<LabelContractSummary[]>([])
  const [catalog, setCatalog] = useState<JourneyCatalogView | null>(null)
  const [target, setTarget] = useState<number | null>(null)
  const [file, setFile] = useState<File | null>(null)
  const [fieldErrors, setFieldErrors] = useState<FieldError[]>([])
  const [message, setMessage] = useState<string | null>(null)
  const [uploading, setUploading] = useState(false)
  const [dragOver, setDragOver] = useState(false)

  useEffect(() => {
    let cancelled = false
    void listLabelContracts()
      .then((items) => {
        if (cancelled) return
        setContracts(items)
        const active = items.find((item) => item.state === 'active')
        setTarget(active ? active.version : (items[0]?.version ?? null))
      })
      .catch(() => undefined)
    void getJourneyCatalog()
      .then((view) => {
        if (!cancelled) setCatalog(view)
      })
      .catch(() => undefined)
    return () => {
      cancelled = true
    }
  }, [])

  function selectFile(next: File | null) {
    setFile(next)
    setFieldErrors([])
    setMessage(null)
  }

  async function submit() {
    if (!file || target === null) return
    setUploading(true)
    setFieldErrors([])
    setMessage(null)
    try {
      const detail = await uploadBusinessSystemConfig({ file, targetLabelContractVersion: target })
      onUploaded(detail.systemKey, detail.id)
    } catch (reason) {
      if (reason instanceof ConfigApiError) {
        setFieldErrors(reason.fieldErrors)
        setMessage(reason.message)
      } else {
        setMessage(reason instanceof Error ? reason.message : '上传失败，请重试。')
      }
    } finally {
      setUploading(false)
    }
  }

  const selectable = contracts.filter((item) => item.state !== 'retired')

  return (
    <div className="overlay-layer" role="dialog" aria-modal="true" aria-label="上传业务系统配置">
      <div className="overlay-scrim" aria-hidden="true" onClick={onClose} />
      <div className="overlay-panel upload-panel">
        <header>
          <h2>上传业务系统配置</h2>
          <button className="icon-button" aria-label="关闭上传层" onClick={onClose}>
            ✕
          </button>
        </header>
        <p className="overlay-hint">
          上传一份严格 YAML 配置；系统身份取自文件根 system_key，首次上传会创建一个 Disabled 业务系统。
          修改停用/启用也通过发布新的 YAML 版本完成。
        </p>
        <div className="overlay-body">
          <div className="form-row">
            <a className="secondary-button compact" href="/api/v1/templates/business-system" download>
              下载模板
            </a>
          </div>
          <div className="form-row">
            <label htmlFor="upload-target-contract">目标 Label Contract</label>
            <select
              id="upload-target-contract"
              value={target ?? ''}
              onChange={(event) => setTarget(event.target.value ? Number(event.target.value) : null)}
            >
              {selectable.length === 0 && <option value="">暂无可用契约版本</option>}
              {selectable.map((item) => (
                <option key={item.id} value={item.version}>
                  v{item.version} · {item.state === 'active' ? '当前激活' : '草稿'}
                </option>
              ))}
            </select>
          </div>
          {catalog && (
            <p className="provenance-note">
              Journey Catalog：<code>{catalog.version}</code> · digest <code>{catalog.digest.slice(0, 16)}…</code>
            </p>
          )}
          <div
            className={`drop-zone ${dragOver ? 'drag-over' : ''}`}
            onDragOver={(event) => {
              event.preventDefault()
              setDragOver(true)
            }}
            onDragLeave={() => setDragOver(false)}
            onDrop={(event) => {
              event.preventDefault()
              setDragOver(false)
              const dropped = event.dataTransfer.files?.[0]
              if (dropped) selectFile(dropped)
            }}
          >
            <label className="file-pick">
              <input
                type="file"
                accept=".yaml,.yml,text/yaml,application/yaml"
                onChange={(event) => selectFile(event.target.files?.[0] ?? null)}
              />
              <span>拖放或选择一份 YAML 文件</span>
            </label>
            {file && (
              <p className="chosen-file" aria-live="polite">
                已选择：<strong>{file.name}</strong>（{file.size} 字节）
              </p>
            )}
          </div>
          {message && (
            <div className="error-summary" role="alert">
              <strong>上传未通过静态校验</strong>
              <span>{message}</span>
            </div>
          )}
          {fieldErrors.length > 0 && (
            <ul className="field-error-list" aria-label="逐项校验错误">
              {fieldErrors.map((item, index) => (
                <li key={`${item.path}-${index}`}>
                  <code>{item.path || 'document'}</code>
                  <span>{item.reason}</span>
                  {item.remediation && <small>{item.remediation}</small>}
                </li>
              ))}
            </ul>
          )}
        </div>
        <footer>
          <button className="secondary-button" onClick={onClose} disabled={uploading}>
            取消
          </button>
          <button
            className="primary-button"
            disabled={uploading || file === null || target === null}
            onClick={() => void submit()}
          >
            {uploading ? '正在上传…' : '上传并校验'}
          </button>
        </footer>
      </div>
    </div>
  )
}
