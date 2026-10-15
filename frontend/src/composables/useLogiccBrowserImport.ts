import { onBeforeUnmount, onMounted, ref } from 'vue'

export const LOGICC_IMPORT_MESSAGE = 'sub2api:logicc-import:v1'
export const LOGICC_IMPORT_ACK = 'sub2api:logicc-import-ack:v1'
const IMPORT_WINDOW_MS = 120_000

export interface LogiccImportedCredentials {
  access_token: string
  client_cookie: string
  session_id: string
}

// The extension runs only after a user gesture. This validation also prevents
// stale popup results, unrelated postMessages and mismatched sessions filling a form.
export function validateLogiccImport(value: unknown): LogiccImportedCredentials | null {
  if (!value || typeof value !== 'object') return null
  const raw = value as Record<string, unknown>
  const { access_token: token, client_cookie: cookie, session_id: session } = raw
  if (typeof token !== 'string' || token.length > 32_768 ||
      typeof cookie !== 'string' || !cookie || cookie.length > 32_768 || /[;\s]/.test(cookie) ||
      typeof session !== 'string' || !/^sess_[A-Za-z0-9_-]{1,150}$/.test(session)) return null
  const parts = token.split('.')
  if (parts.length !== 3 || parts.some(part => !/^[A-Za-z0-9_-]+$/.test(part))) return null
  try {
    const payload = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    const claims = JSON.parse(atob(payload.padEnd(Math.ceil(payload.length / 4) * 4, '=')))
    if (claims.sid !== session || typeof claims.exp !== 'number' || claims.exp * 1000 <= Date.now()) return null
  } catch {
    return null
  }
  return { access_token: token, client_cookie: cookie, session_id: session }
}

export function useLogiccBrowserImport(apply: (credentials: LogiccImportedCredentials) => void) {
  const nonce = ref('')
  const expiresAt = ref(0)
  const status = ref<'idle' | 'waiting' | 'success' | 'expired' | 'invalid'>('idle')
  let timer: ReturnType<typeof setTimeout> | undefined

  function clearPending() {
    nonce.value = ''
    expiresAt.value = 0
    if (timer) clearTimeout(timer)
    timer = undefined
  }

  function start() {
    clearPending()
    nonce.value = crypto.randomUUID()
    expiresAt.value = Date.now() + IMPORT_WINDOW_MS
    status.value = 'waiting'
    timer = setTimeout(() => {
      clearPending()
      status.value = 'expired'
    }, IMPORT_WINDOW_MS)
  }

  function cancel() {
    clearPending()
    status.value = 'idle'
  }

  function receive(event: MessageEvent) {
    if (event.source !== window || event.origin !== window.location.origin ||
        !event.data || typeof event.data !== 'object' || event.data.type !== LOGICC_IMPORT_MESSAGE ||
        !nonce.value || event.data.nonce !== nonce.value || Date.now() >= expiresAt.value) return
    const responseNonce = nonce.value
    const credentials = validateLogiccImport(event.data.credentials)
    clearPending()
    if (credentials) {
      apply(credentials)
      status.value = 'success'
    } else {
      status.value = 'invalid'
    }
    // Acknowledgements contain no credentials. The extension reports success
    // only after the mounted form has accepted the message.
    window.postMessage({ type: LOGICC_IMPORT_ACK, nonce: responseNonce, ok: !!credentials }, window.location.origin)
  }

  onMounted(() => window.addEventListener('message', receive))
  onBeforeUnmount(() => {
    clearPending()
    window.removeEventListener('message', receive)
  })

  return { nonce, expiresAt, status, start, cancel }
}
