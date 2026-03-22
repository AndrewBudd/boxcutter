const API_BASE = process.env.ORCHESTRATOR_API || 'http://localhost:8801'

export async function fetchAPI(path: string, options?: RequestInit) {
  const res = await fetch(`${API_BASE}${path}`, {
    ...options,
    cache: 'no-store',
  })
  if (!res.ok) {
    throw new Error(`API error: ${res.status} ${await res.text()}`)
  }
  const contentType = res.headers.get('content-type') || ''
  if (contentType.includes('application/json')) {
    return res.json()
  }
  return res.text()
}
