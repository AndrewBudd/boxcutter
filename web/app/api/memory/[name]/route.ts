import { fetchAPI } from '@/lib/api'
import { NextResponse, NextRequest } from 'next/server'

async function fetchNodeAgentVM(bridgeIP: string, name: string) {
  try {
    const res = await fetch(`http://${bridgeIP}:8800/api/vms/${name}`, { cache: 'no-store' })
    if (!res.ok) return null
    return res.json()
  } catch {
    return null
  }
}

export async function GET(_req: NextRequest, { params }: { params: Promise<{ name: string }> }) {
  const { name } = await params
  try {
    const [activity, nodes] = await Promise.all([
      fetchAPI(`/api/tapegun/activity/${name}`),
      fetchAPI('/api/nodes'),
    ])

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const node = (nodes as any[]).find((n: { id: string }) => n.id === activity.node_id)
    const vmState = node ? await fetchNodeAgentVM(node.bridge_ip, name) : null

    return NextResponse.json({
      name: activity.name,
      node_id: activity.node_id,
      activity: activity.activity ?? null,
      health: activity.health ?? null,
      pending_messages: activity.pending_messages ?? 0,
      vm_state: vmState,
    })
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
