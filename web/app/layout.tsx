import type { Metadata } from 'next'
import './globals.css'
import Link from 'next/link'

export const metadata: Metadata = {
  title: 'Boxcutter',
  description: 'Ephemeral dev environments on bare metal',
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className="dark">
      <head>
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <link rel="preconnect" href="https://fonts.googleapis.com" />
        <link rel="preconnect" href="https://fonts.gstatic.com" crossOrigin="" />
        <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet" />
      </head>
      <body className="bg-[#0a0a0f] text-gray-100 min-h-screen" style={{ fontFamily: "'Inter', system-ui, sans-serif" }}>
        <nav className="sticky top-0 z-50 bg-[#0a0a0f]/80 backdrop-blur-xl border-b border-white/5 px-4 md:px-6 py-3">
          <div className="max-w-7xl mx-auto flex items-center gap-6">
            <Link href="/" className="flex items-center gap-2 group">
              <div className="w-7 h-7 rounded-lg bg-gradient-to-br from-blue-500 to-purple-600 flex items-center justify-center text-xs font-bold shadow-lg shadow-blue-900/20">B</div>
              <span className="text-base font-semibold text-white group-hover:text-blue-400 transition-colors">Boxcutter</span>
            </Link>
            <div className="flex gap-1">
              <NavLink href="/">VMs</NavLink>
              <NavLink href="/activity">Activity</NavLink>
              <NavLink href="/nodes">Nodes</NavLink>
            </div>
          </div>
        </nav>
        <main className="max-w-7xl mx-auto p-4 md:p-6">{children}</main>
      </body>
    </html>
  )
}

function NavLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="px-3 py-1.5 rounded-lg text-sm text-gray-400 hover:text-white hover:bg-white/5 transition-all duration-150"
    >
      {children}
    </Link>
  )
}
