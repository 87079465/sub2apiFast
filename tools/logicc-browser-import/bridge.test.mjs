import { test } from 'node:test'
import assert from 'node:assert/strict'
import { importSession, isAllowedDestination, readLogiccSession, selectClientCookie, validateSession, validateTarget } from './bridge.mjs'

const token = (sid = 'sess_example', exp = Math.floor(Date.now() / 1000) + 300) =>
  `e30.${Buffer.from(JSON.stringify({ sid, exp })).toString('base64url')}.signature`
const target = () => ({ origin: 'http://127.0.0.1:3000', nonce: '12345678-1234-1234-1234-123456789abc', expiresAt: Date.now() + 120_000 })
const targetTab = { id: 1, url: 'http://127.0.0.1:3000/admin/accounts', incognito: false }

test('requires an HTTPS or local destination and a fresh matching form challenge', () => {
  assert.equal(isAllowedDestination('http://127.0.0.1:3000/admin/accounts'), true)
  assert.equal(isAllowedDestination('https://my-admin.example.com'), true)
  for (const url of ['http://untrusted.example.com', 'https://user:pass@example.com', 'file:///tmp/a', 'chrome://settings']) {
    assert.equal(isAllowedDestination(url), false)
  }
  assert.deepEqual(validateTarget(target(), targetTab.url).origin, target().origin)
  assert.throws(() => validateTarget({ ...target(), expiresAt: Date.now() - 1 }, targetTab.url))
  assert.throws(() => validateTarget({ ...target(), origin: 'https://wrong.example.com' }, targetTab.url))
})

test('rejects expired or mismatched session tokens', () => {
  assert.equal(validateSession({ session_id: 'sess_example', access_token: token() }).session_id, 'sess_example')
  assert.throws(() => validateSession({ session_id: 'sess_example', access_token: token('sess_other') }))
  assert.throws(() => validateSession({ session_id: 'sess_example', access_token: token('sess_example', 1) }))
  assert.throws(() => validateSession({ error: 'not_signed_in' }))
})

test('selects only Logicc cookies and handles partitions without exporting unrelated cookies', () => {
  const cookie = { name: '__client', domain: 'clerk.logicc.com', value: 'sample-cookie' }
  assert.equal(selectClientCookie([{ name: 'other', domain: 'clerk.logicc.com', value: 'no' }, cookie]), cookie.value)
  assert.throws(() => selectClientCookie([{ ...cookie, domain: 'another.example.com' }]))
  assert.throws(() => selectClientCookie([{ ...cookie, partitionKey: { topLevelSite: 'https://another.example.com' } }]))
  assert.throws(() => selectClientCookie([cookie, { ...cookie, value: 'conflicting-cookie' }]))
  assert.equal(selectClientCookie([cookie, { ...cookie, value: 'partitioned-cookie', partitionKey: { topLevelSite: 'https://logicc.com' } }]), 'partitioned-cookie')
})

function apiFixture({ changed = false, rejected = false, loggedOut = false } = {}) {
  const calls = []
  return {
    calls,
    tabs: { get: async id => id === 1 ? { ...targetTab, url: changed ? 'https://different.example.com' : targetTab.url } : { id, url: 'https://app.logicc.com/chat/1', incognito: false } },
    cookies: {
      getAllCookieStores: async () => [{ id: 'wrong-store', tabIds: [99] }, { id: 'right-store', tabIds: [2] }],
      getAll: async query => { calls.push(query); return [{ name: '__client', domain: 'clerk.logicc.com', value: 'sample-cookie' }] }
    },
    scripting: { executeScript: async details => {
      calls.push(details)
      if (details.target.tabId === 2) return [{ result: loggedOut ? { error: 'not_signed_in' } : { session_id: 'sess_example', access_token: token() } }]
      return [{ result: { ok: !rejected } }]
    } }
  }
}

test('reads the selected profile and fills only the armed tab', async () => {
  const api = apiFixture()
  assert.equal(await importSession(api, targetTab, target(), 2), true)
  assert.deepEqual(api.calls[1], { url: 'https://clerk.logicc.com/', name: '__client', storeId: 'right-store' })
  const delivery = api.calls[2]
  assert.equal(delivery.target.tabId, 1)
  assert.deepEqual(Object.keys(delivery.args[1]).sort(), ['access_token', 'client_cookie', 'session_id'])
})

test('does not read cookies after navigation or while logged out, and requires a form acknowledgement', async () => {
  const changed = apiFixture({ changed: true })
  await assert.rejects(importSession(changed, targetTab, target(), 2))
  assert.equal(changed.calls.length, 0)
  const loggedOut = apiFixture({ loggedOut: true })
  await assert.rejects(importSession(loggedOut, targetTab, target(), 2))
  assert.equal(loggedOut.calls.length, 1)
  await assert.rejects(importSession(apiFixture({ rejected: true }), targetTab, target(), 2))
})

test('reads Clerk session from the page only and rejects a session changed during token refresh', async () => {
  const oldWindow = globalThis.window
  const oldLocation = globalThis.location
  try {
    globalThis.location = { origin: 'https://app.logicc.com' }
    globalThis.window = { Clerk: { session: { id: 'sess_example', getToken: async () => token() } } }
    assert.equal((await readLogiccSession()).session_id, 'sess_example')
    window.Clerk.session.getToken = async () => { window.Clerk.session = { id: 'sess_other' }; return token() }
    assert.equal((await readLogiccSession()).error, 'session_changed')
    globalThis.location = { origin: 'https://other.example.com' }
    assert.equal((await readLogiccSession()).error, 'wrong_origin')
  } finally {
    globalThis.window = oldWindow
    globalThis.location = oldLocation
  }
})
