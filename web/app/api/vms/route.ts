import { fetchAPI } from '@/lib/api'
import { NextResponse } from 'next/server'

export async function GET() {
  try {
    const data = await fetchAPI('/api/vms')
    return NextResponse.json(data)
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
