import { fetchAPI } from '@/lib/api'
import { NextResponse, NextRequest } from 'next/server'

export async function PUT(req: NextRequest, { params }: { params: Promise<{ agent: string; key: string }> }) {
  const { agent, key } = await params
  const body = await req.json()
  try {
    const data = await fetchAPI(`/api/knowledge/${agent}/${key}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    return NextResponse.json(data)
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}

export async function DELETE(_req: NextRequest, { params }: { params: Promise<{ agent: string; key: string }> }) {
  const { agent, key } = await params
  try {
    await fetchAPI(`/api/knowledge/${agent}/${key}`, { method: 'DELETE' })
    return new NextResponse(null, { status: 204 })
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
