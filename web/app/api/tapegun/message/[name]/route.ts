import { fetchAPI } from '@/lib/api'
import { NextResponse, NextRequest } from 'next/server'

export async function POST(req: NextRequest, { params }: { params: Promise<{ name: string }> }) {
  const { name } = await params
  const body = await req.json()
  try {
    const data = await fetchAPI(`/api/tapegun/message/${name}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    return NextResponse.json(data)
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
