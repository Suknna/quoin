// Attachment staging API (T14): uploads stream as multipart to the frozen
// POST /api/v1/investigation-attachments route and return the durable
// TextAttachmentSummary the send command references.

export interface TextAttachmentSummary {
  id: string
  artifactId: string
  originalFilename: string
  mediaType: 'text/plain'
  sizeBytes: number
  digest: string
  bodyExpired: boolean
  createdAt: string
}

export function attachmentCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

function problemMessage(payload: unknown, fallback: string): string {
  if (payload && typeof payload === 'object' && 'message' in payload) {
    const message = (payload as { message?: unknown }).message
    if (typeof message === 'string' && message !== '') return message
  }
  return fallback
}

async function readProblem(response: Response, fallback: string): Promise<string> {
  try {
    return problemMessage(await response.json(), fallback)
  } catch {
    return fallback
  }
}

// uploadAttachment streams one file to staging. A network-uncertain retry
// reuses the same command id (HTTP-COMMAND-004); validation failures are
// deterministic and surface in place (UI-CHAT-006).
export async function uploadAttachment(file: File, commandId: string): Promise<TextAttachmentSummary> {
  const body = new FormData()
  body.append('clientCommandId', commandId)
  body.append('file', file, file.name)
  const response = await fetch('/api/v1/investigation-attachments', {
    method: 'POST',
    credentials: 'include',
    body,
  })
  if (!response.ok) {
    throw new Error(await readProblem(response, '附件上传失败，请重试。'))
  }
  return (await response.json()) as TextAttachmentSummary
}

