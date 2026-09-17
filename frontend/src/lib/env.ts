export const API_BASE = (import.meta.env.VITE_API_BASE || '/api/v1').replace(/\/$/, '')

export function streamUrl(): string {
  const explicit = import.meta.env.VITE_WS_URL
  if (explicit) return explicit
  if (/^https?:\/\//.test(API_BASE)) return API_BASE.replace(/^http/, 'ws') + '/stream'
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${window.location.host}${API_BASE}/stream`
}

export const WALLETCONNECT_ID: string = import.meta.env.VITE_WALLETCONNECT_ID || ''
