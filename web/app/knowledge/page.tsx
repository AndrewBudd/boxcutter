'use client'

import { useEffect, useState } from 'react'

interface KnowledgeEntry {
  agent: string
  key: string
  value: string
  updated_at: string
}

function formatTime(ts: string): string {
  try {
    return new Date(ts).toLocaleString()
  } catch {
    return ts
  }
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="text-[10px] font-semibold uppercase tracking-widest text-gray-600 mb-2">
      {children}
    </div>
  )
}

function AgentGroup({
  agent,
  entries,
  onDelete,
}: {
  agent: string
  entries: KnowledgeEntry[]
  onDelete: (agent: string, key: string) => void
}) {
  return (
    <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl overflow-hidden">
      {/* Group header */}
      <div className="flex items-center gap-2 px-4 py-3 border-b border-white/5 bg-white/[0.02]">
        <span
          className="font-medium text-white"
          style={{ fontFamily: "'JetBrains Mono', monospace" }}
        >
          {agent}
        </span>
        <span className="text-[10px] px-1.5 py-0.5 rounded bg-blue-500/10 text-blue-400 border border-blue-500/15 font-mono">
          {entries.length} {entries.length === 1 ? 'entry' : 'entries'}
        </span>
      </div>

      {/* Entries table */}
      <div className="divide-y divide-white/5">
        {entries.map((entry) => (
          <div key={entry.key} className="flex items-start gap-3 px-4 py-3 group hover:bg-white/[0.015]">
            <div className="flex-1 grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-x-4 gap-y-0.5 min-w-0">
              <div
                className="text-sm font-medium text-purple-300 truncate"
                style={{ fontFamily: "'JetBrains Mono', monospace" }}
                title={entry.key}
              >
                {entry.key}
              </div>
              <div
                className="text-sm text-gray-300 leading-relaxed break-words"
                style={{ fontFamily: "'JetBrains Mono', monospace" }}
              >
                {entry.value}
              </div>
              <div className="text-[10px] text-gray-700 col-start-1">
                updated {formatTime(entry.updated_at)}
              </div>
            </div>
            <button
              onClick={() => onDelete(entry.agent, entry.key)}
              className="flex-shrink-0 text-xs px-2 py-1 rounded text-gray-600 hover:text-red-400 hover:bg-red-500/10 border border-transparent hover:border-red-500/20 transition-all duration-150 opacity-0 group-hover:opacity-100"
              title={`Delete ${entry.key}`}
            >
              delete
            </button>
          </div>
        ))}
      </div>
    </div>
  )
}

function AddEntryForm({ onAdd }: { onAdd: () => void }) {
  const [agent, setAgent] = useState('')
  const [key, setKey] = useState('')
  const [value, setValue] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!agent.trim() || !key.trim() || !value.trim()) return
    setSaving(true)
    setError('')
    try {
      const res = await fetch(
        `/api/knowledge/${encodeURIComponent(agent.trim())}/${encodeURIComponent(key.trim())}`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ value: value.trim() }),
        }
      )
      if (!res.ok) {
        const text = await res.text()
        setError(text || 'Failed to save')
        return
      }
      setAgent('')
      setKey('')
      setValue('')
      onAdd()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Network error')
    } finally {
      setSaving(false)
    }
  }

  const inputCls =
    'flex-1 min-w-0 bg-gray-900/60 border border-gray-800 rounded-lg px-3 py-2 text-sm text-gray-200 placeholder-gray-600 focus:outline-none focus:border-blue-500/50 focus:bg-gray-900 transition-colors font-mono'

  return (
    <form onSubmit={handleSubmit}>
      <SectionLabel>Add / Update Entry</SectionLabel>
      <div className="flex flex-wrap gap-2 mb-2">
        <input
          className={inputCls}
          style={{ minWidth: '120px', maxWidth: '220px' }}
          placeholder="agent"
          value={agent}
          onChange={(e) => setAgent(e.target.value)}
          disabled={saving}
        />
        <input
          className={inputCls}
          style={{ minWidth: '120px', maxWidth: '220px' }}
          placeholder="key"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          disabled={saving}
        />
        <input
          className={inputCls}
          style={{ flex: '2 1 200px' }}
          placeholder="value"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          disabled={saving}
        />
        <button
          type="submit"
          disabled={saving || !agent.trim() || !key.trim() || !value.trim()}
          className="px-4 py-2 rounded-lg text-sm font-medium bg-blue-600/80 hover:bg-blue-500/80 disabled:opacity-40 disabled:cursor-not-allowed text-white transition-colors border border-blue-500/30"
        >
          {saving ? 'Saving…' : 'Save'}
        </button>
      </div>
      {error && (
        <div className="text-xs text-red-400 mt-1">{error}</div>
      )}
    </form>
  )
}

export default function KnowledgePage() {
  const [entries, setEntries] = useState<KnowledgeEntry[]>([])
  const [loaded, setLoaded] = useState(false)
  const [deleteError, setDeleteError] = useState('')

  async function fetchEntries() {
    try {
      const data = await fetch('/api/knowledge').then((r) => r.json())
      if (Array.isArray(data)) {
        setEntries(data)
      }
      setLoaded(true)
    } catch {
      setLoaded(true)
    }
  }

  useEffect(() => {
    fetchEntries()
    const id = setInterval(fetchEntries, 15000)
    return () => clearInterval(id)
  }, [])

  async function handleDelete(agent: string, key: string) {
    setDeleteError('')
    try {
      const res = await fetch(
        `/api/knowledge/${encodeURIComponent(agent)}/${encodeURIComponent(key)}`,
        { method: 'DELETE' }
      )
      if (!res.ok) {
        const text = await res.text()
        setDeleteError(text || 'Failed to delete')
        return
      }
      setEntries((prev) => prev.filter((e) => !(e.agent === agent && e.key === key)))
    } catch (e: unknown) {
      setDeleteError(e instanceof Error ? e.message : 'Network error')
    }
  }

  // Group entries by agent
  const grouped: Record<string, KnowledgeEntry[]> = {}
  for (const entry of entries) {
    if (!grouped[entry.agent]) grouped[entry.agent] = []
    grouped[entry.agent].push(entry)
  }
  const agents = Object.keys(grouped).sort()

  const totalEntries = entries.length

  return (
    <div>
      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-semibold">Knowledge Base</h1>
          <p className="text-sm text-gray-500 mt-0.5">Shared learnings across agents</p>
        </div>
        {loaded && totalEntries > 0 && (
          <div className="flex items-center gap-3 text-xs text-gray-500">
            <span className="flex items-center gap-1.5">
              <span className="w-2 h-2 rounded-full bg-gray-700 inline-block" />
              {agents.length} {agents.length === 1 ? 'agent' : 'agents'}
            </span>
            <span className="flex items-center gap-1.5">
              <span className="w-2 h-2 rounded-full bg-blue-600 inline-block" />
              {totalEntries} {totalEntries === 1 ? 'entry' : 'entries'}
            </span>
          </div>
        )}
      </div>

      {/* Add entry form */}
      <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl p-4 mb-6">
        <AddEntryForm onAdd={fetchEntries} />
      </div>

      {/* Delete error */}
      {deleteError && (
        <div className="mb-4 bg-red-500/10 border border-red-500/20 rounded-xl px-4 py-3 text-sm text-red-400">
          {deleteError}
        </div>
      )}

      {/* Content */}
      {!loaded && (
        <div className="text-center py-16 text-gray-600">Loading...</div>
      )}
      {loaded && agents.length === 0 && (
        <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl py-16 text-center text-gray-600">
          No knowledge entries yet. Add one above.
        </div>
      )}
      {agents.length > 0 && (
        <div className="flex flex-col gap-4">
          {agents.map((agent) => (
            <AgentGroup
              key={agent}
              agent={agent}
              entries={grouped[agent]}
              onDelete={handleDelete}
            />
          ))}
        </div>
      )}
    </div>
  )
}
