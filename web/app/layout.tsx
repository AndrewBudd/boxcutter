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
      <body className="bg-gray-950 text-gray-100 min-h-screen">
        <div className="flex min-h-screen">
          <nav className="w-48 bg-gray-900 border-r border-gray-800 p-4 flex flex-col gap-1">
            <Link href="/" className="text-lg font-bold text-white mb-4 block">Boxcutter</Link>
            <NavLink href="/">Dashboard</NavLink>
            <NavLink href="/activity">Activity</NavLink>
            <NavLink href="/nodes">Nodes</NavLink>
          </nav>
          <main className="flex-1 p-6">{children}</main>
        </div>
      </body>
    </html>
  )
}

function NavLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="block px-3 py-2 rounded text-sm text-gray-300 hover:bg-gray-800 hover:text-white transition-colors"
    >
      {children}
    </Link>
  )
}
