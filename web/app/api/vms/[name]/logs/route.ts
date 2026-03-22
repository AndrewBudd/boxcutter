import { fetchAPI } from '@/lib/api'
import { NextResponse, NextRequest } from 'next/server'

export async function GET(req: NextRequest, { params }: { params: Promise<{ name: string }> }) {
  const { name } = await params
  const lines = req.nextUrl.searchParams.get('lines') || '100'
  try {
    const data = await fetchAPI(`/api/vms/${name}/logs?lines=${lines}`)
    return new NextResponse(data, { headers: { 'Content-Type': 'text/plain' } })
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
