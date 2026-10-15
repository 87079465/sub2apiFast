# Logicc browser import (Chrome / Edge)

This extension fills a Sub2API Logicc account form from a Logicc tab you have
already signed into. It does not create or save an account automatically.

## 安装与使用

1. Chrome 打开 `chrome://extensions`；Edge 打开 `edge://extensions`。
2. 启用 **开发者模式**，点击 **加载已解压的扩展程序**。
3. 选择本目录（包含 `manifest.json`），然后把扩展固定到工具栏。
   也可以从后台 Logicc 账号表单下载 ZIP，解压后选择解压目录。
4. 在**同一个浏览器配置**中保持 Logicc 已登录，并打开 Sub2API 后台。
   本地地址：`http://127.0.0.1:3000`。不要在 Codex 内置浏览器里发起导入。
5. 打开新增或编辑 Logicc 账号表单，点击 **从浏览器导入**。
6. 点击工具栏扩展图标，确认显示的后台地址，选择 Logicc 标签页，点击
   **读取并填入**。如有多个 Logicc 标签页，请选择你要使用的登录会话。
7. 关闭扩展，检查表单并保存账号。助手 ID 等已有字段不会被导入覆盖。

等待窗口有效期为两分钟。超时、关闭表单或切换后台页面后，需要重新开始。
扩展只能读取安装它的浏览器配置中的标签页，不能读取其他浏览器或其他用户
配置的会话。初次安装后若浏览器提示无法访问页面，刷新后台和 Logicc 标签页。

## 权限与数据流

- `activeTab`: only when you open the extension, inspect and fill the current
  admin tab. Remote destinations must use HTTPS; HTTP is allowed only on loopback.
- `scripting`: read `window.Clerk.session` in the selected Logicc tab, and send
  credentials to the waiting admin form. No continuous content scripts run.
- `cookies`: read only the `__client` cookie applicable to `clerk.logicc.com`
  from the selected tab's cookie store, including HttpOnly cookies.
- Persistent host access is limited to `app.logicc.com` and `clerk.logicc.com`.
  There is no `<all_urls>` permission, background worker, credential storage,
  clipboard access or extension-origin network request.

The user clicks the extension's import button before any session/cookie is read.
The Logicc page uses its own Clerk SDK to obtain a current Session JWT. The
extension verifies the JWT session ID, selects the matching browser cookie store,
then delivers the three fields directly to the chosen tab via an origin-bound,
expiring, one-use form nonce. The popup reports success only after the form
acknowledges receipt. It never displays the credential values or prints them to
logs. The account is saved through Sub2API's normal authenticated admin API only
when the user submits the form.

## Development

Run `node --test tools/logicc-browser-import/bridge.test.mjs` from the repository
root. After changing extension files, reload it on the browser Extensions page.
The download ZIP contains only `manifest.json`, `popup.html`, `popup.css`,
`popup.mjs`, `bridge.mjs` and this README; tests and real credentials are excluded.
