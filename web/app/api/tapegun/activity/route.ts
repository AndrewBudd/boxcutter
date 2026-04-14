import { fetchAPI } from '@/lib/api'
import { NextResponse } from 'next/server'

export async function GET() {
  try {
    const data = await fetchAPI('/api/tapegun/activity')
    // Strip sensitive fields before sending to browser
    if (Array.isArray(data)) {
      for (const entry of data) {
        if (entry.agent_config?.inference?.api_key) {
          entry.agent_config.inference.api_key = '***'
        }
        if (entry.agent_config?.claude?.api_key) {
          entry.agent_config.claude.api_key = '***'
        }
      }
    }
    return NextResponse.json(data)
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
