import { fetchAPI } from '@/lib/api'
import { NextResponse } from 'next/server'

async function fetchNodeAgentVM(bridgeIP: string, name: string) {
  try {
    const res = await fetch(`http://${bridgeIP}:8800/api/vms/${name}`, { cache: 'no-store' })
    if (!res.ok) return null
    return res.json()
  } catch {
    return null
  }
}

export async function GET() {
  try {
    const [activity, nodes] = await Promise.all([
      fetchAPI('/api/tapegun/activity'),
      fetchAPI('/api/nodes'),
    ])

    const nodeMap: Record<string, { bridge_ip: string }> = Object.fromEntries(
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (nodes as any[]).map((n: { id: string; bridge_ip: string }) => [n.id, n])
    )

    const summaries = await Promise.all(
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (activity as any[]).map(async (vm: any) => {
        const node = nodeMap[vm.node_id]
        const vmState = node ? await fetchNodeAgentVM(node.bridge_ip, vm.name) : null

        return {
          name: vm.name,
          node_id: vm.node_id,
          activity: vm.activity ?? null,
          health: vm.health ?? null,
          pending_messages: vm.pending_messages ?? 0,
          vm_state: vmState,
        }
      })
    )

    return NextResponse.json(summaries)
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    return NextResponse.json({ error: msg }, { status: 502 })
  }
}
