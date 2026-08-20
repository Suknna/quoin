import { useState } from 'react'
import { discoverProviderModels, type DiscoveredModel } from './api'

// Model provider creation form (T08): discovery as an input helper with
// the mandatory manual fallback — a failed discovery never blocks creating
// the connection by hand.

export interface ModelProviderFormValue {
  baseUrl: string
  apiKey: string
  chatModelId: string
  embeddingModelId: string
  contextBudgetTokens: string
  maxOutputTokens: string
}

export function ModelProviderFields({
  value,
  onChange,
}: {
  value: ModelProviderFormValue
  onChange: (next: ModelProviderFormValue) => void
}) {
  const [discovering, setDiscovering] = useState(false)
  const [discovered, setDiscovered] = useState<DiscoveredModel[] | null>(null)
  const [discoveryNote, setDiscoveryNote] = useState('')

  async function runDiscovery() {
    setDiscovering(true)
    setDiscoveryNote('')
    try {
      const result = await discoverProviderModels(value.baseUrl, value.apiKey)
      setDiscovered(result.models)
      setDiscoveryNote(
        result.available
          ? `发现 ${result.models.length} 个模型；能力以探测结果为准。`
          : `${result.detail ?? '未发现模型'}可以直接手工填写模型 ID。`,
      )
    } catch {
      setDiscovered(null)
      setDiscoveryNote('模型发现暂时不可用；可以直接手工填写模型 ID。')
    } finally {
      setDiscovering(false)
    }
  }

  return (
    <>
      <label>
        Base URL（OpenAI 兼容入口）
        <input
          value={value.baseUrl}
          onChange={(event) => onChange({ ...value, baseUrl: event.target.value })}
          required
          placeholder="https://api.example.com"
        />
      </label>
      <label>
        API Key（一次性提交，保存后不可查看）
        <input
          type="password"
          value={value.apiKey}
          onChange={(event) => onChange({ ...value, apiKey: event.target.value })}
          required
          autoComplete="new-password"
        />
      </label>
      <div className="admin-action-row">
        <button type="button" className="text-button" onClick={runDiscovery} disabled={discovering || !value.baseUrl}>
          {discovering ? '正在发现模型…' : '从该地址发现模型'}
        </button>
        <span className="admin-muted">发现只辅助选择，不构成能力证明</span>
      </div>
      {discoveryNote && (
        <p className="admin-muted" role="status">
          {discoveryNote}
        </p>
      )}
      {discovered && discovered.length > 0 && (
        <label>
          对话模型
          <select
            value={value.chatModelId}
            onChange={(event) => onChange({ ...value, chatModelId: event.target.value })}
          >
            <option value="">手工填写…</option>
            {discovered.map((model) => (
              <option key={model.id} value={model.id}>
                {model.id}
              </option>
            ))}
          </select>
        </label>
      )}
      <label>
        对话模型 ID
        <input
          value={value.chatModelId}
          onChange={(event) => onChange({ ...value, chatModelId: event.target.value })}
          required
          placeholder="gpt-example-chat"
          list="t08-chat-models"
        />
        <datalist id="t08-chat-models">
          {(discovered ?? []).map((model) => (
            <option key={model.id} value={model.id} />
          ))}
        </datalist>
      </label>
      <label>
        Embedding 模型 ID（可选）
        <input
          value={value.embeddingModelId}
          onChange={(event) => onChange({ ...value, embeddingModelId: event.target.value })}
          placeholder="text-example-embed"
          list="t08-embed-models"
        />
        <datalist id="t08-embed-models">
          {(discovered ?? []).map((model) => (
            <option key={model.id} value={model.id} />
          ))}
        </datalist>
      </label>
      <label>
        Context 预算 tokens
        <input
          type="number"
          min={1}
          value={value.contextBudgetTokens}
          onChange={(event) => onChange({ ...value, contextBudgetTokens: event.target.value })}
          required
          placeholder="8192"
        />
      </label>
      <label>
        最大输出 tokens
        <input
          type="number"
          min={1}
          value={value.maxOutputTokens}
          onChange={(event) => onChange({ ...value, maxOutputTokens: event.target.value })}
          required
          placeholder="1024"
        />
      </label>
    </>
  )
}
