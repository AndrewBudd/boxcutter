import { fetchAPI } from '@/lib/api'
import { NextResponse, NextRequest } from 'next/server'

export async function GET(_req: NextRequest, { params }: { params: Promise<{ agent: string }> }) {
  const { agent } = await params
  try {
    const data = await fetchAPI(`/api/knowledge/${agent}`)
    return NextResponse.json(data)
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
