import { importSession, isAllowedDestination, readImportTarget, validateTarget } from './bridge.mjs'

const destination = document.querySelector('#destination')
const sourceSelect = document.querySelector('#source')
const button = document.querySelector('#import')
const status = document.querySelector('#status')
let targetTab
let target

function setStatus(text, state = '') {
  status.textContent = text
  status.dataset.state = state
}

async function initialize() {
  [targetTab] = await chrome.tabs.query({ active: true, currentWindow: true })
  if (!targetTab?.id || !isAllowedDestination(targetTab.url)) throw new Error('请先打开你自己的 Sub2API 后台账号表单，再点击扩展图标。')
  destination.textContent = new URL(targetTab.url).origin
  const [result] = await chrome.scripting.executeScript({ target: { tabId: targetTab.id }, func: readImportTarget })
  target = validateTarget(result?.result, targetTab.url)
  const sources = (await chrome.tabs.query({ url: 'https://app.logicc.com/*' }))
    .filter(tab => tab.incognito === targetTab.incognito)
  sourceSelect.replaceChildren()
  if (!sources.length) throw new Error('没有找到 Logicc 标签页。请在这个浏览器中打开 app.logicc.com 并登录，再打开本扩展。')
  for (const tab of sources) {
    const option = document.createElement('option')
    option.value = String(tab.id)
    option.textContent = `${tab.title || 'Logicc'} · 标签页 ${tab.id}`
    sourceSelect.append(option)
  }
  sourceSelect.disabled = false
  button.disabled = false
  setStatus('选择要导入的 Logicc 标签页，然后点击“读取并填入”。')
}

button.addEventListener('click', async () => {
  button.disabled = true
  sourceSelect.disabled = true
  setStatus('正在读取登录会话并填入表单…')
  try {
    await importSession(chrome, targetTab, target, Number(sourceSelect.value))
    setStatus('已填入！关闭扩展，检查账号表单后点击保存。', 'success')
    button.textContent = '已填入账号表单'
  } catch (error) {
    // Only show our own fixed messages; Chrome errors can contain tab URLs.
    const safe = error instanceof Error && /[\u3400-\u9fff]/.test(error.message)
    setStatus(safe ? error.message : '导入失败，请刷新 Logicc 页面并重新开始导入。', 'error')
    button.disabled = false
    sourceSelect.disabled = false
  }
})

initialize().catch(error => {
  const safe = error instanceof Error && /[\u3400-\u9fff]/.test(error.message)
  setStatus(safe ? error.message : '无法读取当前页面。请刷新后台，再点击“从浏览器导入”。', 'error')
})
