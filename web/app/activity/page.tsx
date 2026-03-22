'use client'

import { useEffect, useState } from 'react'
import Link from 'next/link'
import type { TapegunActivity } from '@/lib/types'

export default function ActivityPage() {
  const [activities, setActivities] = useState<TapegunActivity[]>([])

  useEffect(() => {
    const poll = async () => {
      try {
        const data = await fetch('/api/tapegun/activity').then(r => r.json())
        setActivities(Array.isArray(data) ? data : [])
      } catch {}
    }
    poll()
    const interval = setInterval(poll, 5000)
    return () => clearInterval(interval)
  }, [])

  return (
    <div>
      <h1 className="text-2xl font-bold mb-6">Activity</h1>
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        {activities.map(act => (
          <div key={act.name} className="bg-gray-900 border border-gray-800 rounded-lg overflow-hidden">
            <div className="flex items-center justify-between px-4 py-2 bg-gray-800/50 border-b border-gray-800">
              <Link href={`/vms/${act.name}`} className="font-medium text-blue-400 hover:underline">
                {act.name}
              </Link>
              <div className="flex items-center gap-2 text-xs">
                <span className="text-gray-500">{act.node_name}</span>
                {act.pending_messages > 0 && (
                  <span className="bg-red-600 text-white px-1.5 py-0.5 rounded-full">{act.pending_messages}</span>
                )}
                <span className={`w-2 h-2 rounded-full ${act.vm_status === 'running' ? 'bg-green-500' : 'bg-gray-500'}`} />
              </div>
            </div>
            <div className="p-3 font-mono text-xs text-green-400 whitespace-pre-wrap max-h-[300px] overflow-y-auto bg-[#0d1117]">
              {act.activity?.pane_content || (
                <span className="text-gray-600">No activity data</span>
              )}
            </div>
            {act.activity?.status && (
              <div className="px-4 py-1.5 text-xs text-gray-500 border-t border-gray-800">
                {act.activity.status}
                {act.activity.timestamp && (
                  <span className="ml-2">{new Date(act.activity.timestamp).toLocaleTimeString()}</span>
                )}
              </div>
            )}
          </div>
        ))}
        {activities.length === 0 && (
          <div className="col-span-2 text-center py-12 text-gray-500">No VMs with activity data</div>
        )}
      </div>
    </div>
  )
}
