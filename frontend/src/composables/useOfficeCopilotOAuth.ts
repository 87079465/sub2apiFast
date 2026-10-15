import { ref } from 'vue'
import { apiClient } from '@/api/client'
import { useAppStore } from '@/stores/app'

export interface OfficeCopilotTokenInfo {
  access_token: string
  refresh_token: string
  client_id: string
  expires_in: number
  expires_at: number
  scenario: string
}

export function useOfficeCopilotOAuth() {
  const appStore = useAppStore()
  const authUrl = ref('')
  const sessionId = ref('')
  const userCode = ref('')
  const loading = ref(false)
  const error = ref('')

  const resetState = () => {
    authUrl.value = ''
    sessionId.value = ''
    userCode.value = ''
    loading.value = false
    error.value = ''
  }

  const generateAuthUrl = async (proxyId?: number | null): Promise<boolean> => {
    loading.value = true
    error.value = ''
    try {
      const payload: Record<string, unknown> = {}
      if (proxyId) payload.proxy_id = proxyId
      const { data } = await apiClient.post<{
        user_code: string
        device_code: string
        verification_uri: string
      }>('/admin/office-copilot/oauth/device-code', payload)

      const uri = data.verification_uri || 'https://www.microsoft.com/link'
      authUrl.value = `${uri}  (设备验证码: ${data.user_code})`
      sessionId.value = data.device_code
      userCode.value = data.user_code
      try {
        window.open(uri, '_blank', 'noopener,noreferrer')
      } catch {
        // ignore popup blocker
      }
      return true
    } catch (err: any) {
      error.value = err.response?.data?.message || err.response?.data?.detail || '获取微软设备验证码失败'
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  const exchangeDeviceCode = async (proxyId?: number | null): Promise<OfficeCopilotTokenInfo | null> => {
    if (!sessionId.value) {
      error.value = '请先点击生成设备验证码'
      return null
    }
    loading.value = true
    error.value = ''
    try {
      const payload: Record<string, unknown> = { device_code: sessionId.value }
      if (proxyId) payload.proxy_id = proxyId
      const { data } = await apiClient.post<OfficeCopilotTokenInfo>('/admin/office-copilot/oauth/exchange-token', payload)
      return data
    } catch (err: any) {
      error.value = err.response?.data?.message || err.response?.data?.detail || '兑换 Office Copilot 令牌失败'
      appStore.showError(error.value)
      return null
    } finally {
      loading.value = false
    }
  }

  const validateRefreshToken = async (
    refreshToken: string,
    proxyId?: number | null
  ): Promise<OfficeCopilotTokenInfo | null> => {
    loading.value = true
    error.value = ''
    try {
      const payload: Record<string, unknown> = { refresh_token: refreshToken.trim() }
      if (proxyId) payload.proxy_id = proxyId
      const { data } = await apiClient.post<OfficeCopilotTokenInfo>('/admin/office-copilot/oauth/exchange-token', payload)
      return data
    } catch (err: any) {
      error.value = err.response?.data?.message || err.response?.data?.detail || '校验 Refresh Token 失败'
      appStore.showError(error.value)
      return null
    } finally {
      loading.value = false
    }
  }

  return {
    authUrl,
    sessionId,
    userCode,
    loading,
    error,
    resetState,
    generateAuthUrl,
    exchangeDeviceCode,
    validateRefreshToken
  }
}
