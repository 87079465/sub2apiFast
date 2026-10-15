export function isAllowedDestination(raw) {
  try {
    const url = new URL(raw)
    if (url.username || url.password) return false
    return url.protocol === 'https:' || (url.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname))
  } catch {
    return false
  }
}

export function validateTarget(target, url, now = Date.now()) {
  if (!isAllowedDestination(url) || !target || target.origin !== new URL(url).origin ||
      typeof target.nonce !== 'string' || !/^[a-zA-Z0-9-]{32,64}$/.test(target.nonce) ||
      !Number.isFinite(target.expiresAt) || target.expiresAt <= now || target.expiresAt > now + 125_000) {
    throw new Error('请在 Sub2API 的 Logicc 账号表单中点击“从浏览器导入”，再打开扩展。')
  }
  return { origin: target.origin, nonce: target.nonce, expiresAt: target.expiresAt }
}

export function validateSession(session, now = Date.now()) {
  if (!session || typeof session.session_id !== 'string' || !/^sess_[A-Za-z0-9_-]{1,150}$/.test(session.session_id) ||
      typeof session.access_token !== 'string' || session.access_token.length > 32_768) {
    throw new Error('未找到有效的 Logicc 登录会话，请刷新 Logicc 标签页后重试。')
  }
  try {
    const parts = session.access_token.split('.')
    if (parts.length !== 3 || parts.some(p => !/^[A-Za-z0-9_-]+$/.test(p))) throw new Error()
    const payload = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    const claims = JSON.parse(atob(payload.padEnd(Math.ceil(payload.length / 4) * 4, '=')))
    if (claims.sid !== session.session_id || typeof claims.exp !== 'number' || claims.exp * 1000 <= now) throw new Error()
  } catch {
    throw new Error('Logicc 会话令牌已过期或不匹配，请刷新登录页面后重试。')
  }
  return { access_token: session.access_token, session_id: session.session_id }
}

export function selectClientCookie(cookies) {
  const eligible = cookies.filter(cookie => cookie.name === '__client' &&
    ['clerk.logicc.com', '.clerk.logicc.com', 'logicc.com', '.logicc.com'].includes(cookie.domain) &&
    typeof cookie.value === 'string' && cookie.value.length > 0 && cookie.value.length <= 32_768 && !/[;\s]/.test(cookie.value) &&
    (!cookie.partitionKey || cookie.partitionKey.topLevelSite === 'https://logicc.com' || cookie.partitionKey.topLevelSite === 'https://app.logicc.com'))
  // Partitioned cookies associated with the Logicc top-level page take precedence.
  const partitioned = eligible.filter(cookie => cookie.partitionKey)
  const candidates = partitioned.length ? partitioned : eligible
  const values = [...new Set(candidates.map(cookie => cookie.value))]
  if (values.length !== 1) throw new Error(values.length ? '检测到多个不同的 Logicc Cookie，请刷新登录页面后重试。' : '找不到 __client Cookie。请在同一浏览器配置中重新登录 Logicc 后重试。')
  return values[0]
}

// These functions are injected with chrome.scripting; they must be self-contained.
export function readImportTarget() {
  if (window.top !== window) return null
  const markers = document.querySelectorAll('[data-sub2api-logicc-import="v1"]')
  if (markers.length !== 1) return null
  const marker = markers[0]
  return { origin: location.origin, nonce: marker.getAttribute('data-nonce'), expiresAt: Number(marker.getAttribute('data-expires-at')) }
}

export async function readLogiccSession() {
  if (location.origin !== 'https://app.logicc.com') return { error: 'wrong_origin' }
  const session = window.Clerk?.session
  if (!session?.id || typeof session.getToken !== 'function') return { error: 'not_signed_in' }
  let timer
  try {
    const token = await Promise.race([
      session.getToken({ skipCache: true }),
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('timeout')), 10_000) })
    ])
    if (window.Clerk?.session?.id !== session.id) return { error: 'session_changed' }
    return { session_id: session.id, access_token: token }
  } catch {
    // Do not return raw errors from the page, which may include private values.
    return { error: 'token_unavailable' }
  } finally {
    clearTimeout(timer)
  }
}

export function deliverCredentials(target, credentials) {
  return new Promise(resolve => {
    const marker = document.querySelector('[data-sub2api-logicc-import="v1"]')
    if (location.origin !== target.origin || !marker || marker.getAttribute('data-nonce') !== target.nonce ||
        Number(marker.getAttribute('data-expires-at')) <= Date.now() || target.expiresAt <= Date.now()) {
      resolve({ ok: false, error: 'target_changed' })
      return
    }
    let timer
    const finish = result => {
      window.removeEventListener('message', receive)
      clearTimeout(timer)
      resolve(result)
    }
    const receive = event => {
      if (event.source === window && event.origin === target.origin && event.data?.type === 'sub2api:logicc-import-ack:v1' && event.data.nonce === target.nonce) {
        finish({ ok: event.data.ok === true, error: event.data.ok ? undefined : 'rejected' })
      }
    }
    window.addEventListener('message', receive)
    timer = setTimeout(() => finish({ ok: false, error: 'no_acknowledgement' }), 4_000)
    window.postMessage({ type: 'sub2api:logicc-import:v1', nonce: target.nonce, credentials }, target.origin)
  })
}

export async function importSession(api, targetTab, target, sourceTabId) {
  const currentTarget = await api.tabs.get(targetTab.id)
  if (currentTarget.url !== targetTab.url) throw new Error('后台页面已切换，请重新点击“从浏览器导入”。')
  validateTarget(target, currentTarget.url)
  const source = await api.tabs.get(sourceTabId)
  if (new URL(source.url).origin !== 'https://app.logicc.com' || source.incognito !== targetTab.incognito) {
    throw new Error('请选择同一浏览器配置中已登录的 Logicc 标签页。')
  }
  const stores = await api.cookies.getAllCookieStores()
  const store = stores.find(item => item.tabIds.includes(sourceTabId))
  if (!store) throw new Error('无法确定 Logicc 标签页的 Cookie 存储。')
  const [result] = await api.scripting.executeScript({ target: { tabId: sourceTabId }, world: 'MAIN', func: readLogiccSession })
  const session = validateSession(result?.result)
  const cookies = await api.cookies.getAll({ url: 'https://clerk.logicc.com/', name: '__client', storeId: store.id })
  const credentials = { ...session, client_cookie: selectClientCookie(cookies) }
  const [delivered] = await api.scripting.executeScript({
    target: { tabId: targetTab.id }, func: deliverCredentials, args: [target, credentials]
  })
  if (!delivered?.result?.ok) throw new Error('表单未接收凭据。请保持账号表单打开，重新点击“从浏览器导入”后重试。')
  return true
}
