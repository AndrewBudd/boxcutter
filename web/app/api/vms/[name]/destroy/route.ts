import { fetchAPI } from '@/lib/api'
import { NextResponse, NextRequest } from 'next/server'

export async function POST(_req: NextRequest, { params }: { params: Promise<{ name: string }> }) {
  const { name } = await params
  try {
    await fetchAPI(`/api/vms/${name}`, { method: 'DELETE' })
    return NextResponse.json({ status: 'destroyed' })
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
