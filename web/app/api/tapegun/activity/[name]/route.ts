import { fetchAPI } from '@/lib/api'
import { NextResponse, NextRequest } from 'next/server'

export async function GET(_req: NextRequest, { params }: { params: Promise<{ name: string }> }) {
  const { name } = await params
  try {
    const data = await fetchAPI(`/api/tapegun/activity/${name}`)
    return NextResponse.json(data)
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
