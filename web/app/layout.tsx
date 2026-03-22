import type { Metadata } from 'next'
import './globals.css'
import Link from 'next/link'

export const metadata: Metadata = {
  title: 'Boxcutter - Tapegun',
  description: 'VM management and monitoring',
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className="dark">
      <head>
        <meta name="viewport" content="width=device-width, initial-scale=1" />
      </head>
      <body className="bg-gray-950 text-gray-100 min-h-screen">
        <nav className="bg-gray-900 border-b border-gray-800 px-4 py-3 flex items-center gap-4 overflow-x-auto">
          <Link href="/" className="text-lg font-bold text-white whitespace-nowrap">Boxcutter</Link>
          <div className="flex gap-1">
            <NavLink href="/">Dashboard</NavLink>
            <NavLink href="/activity">Activity</NavLink>
            <NavLink href="/nodes">Nodes</NavLink>
          </div>
        </nav>
        <main className="p-4 md:p-6">{children}</main>
      </body>
    </html>
  )
}

function NavLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="px-3 py-1.5 rounded text-sm text-gray-300 hover:bg-gray-800 hover:text-white transition-colors whitespace-nowrap"
    >
      {children}
    </Link>
  )
}
