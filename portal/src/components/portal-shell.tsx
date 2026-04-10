"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Activity, AlertTriangle, Cloud, FileText, Gauge } from "lucide-react";

const navItems = [
  { href: "/", label: "Overview", icon: Gauge },
  { href: "/activity", label: "Activity", icon: Activity },
  { href: "/errors", label: "Errors", icon: AlertTriangle },
  { href: "/logs", label: "Logs", icon: FileText },
];

export function PortalShell({
  children,
}: {
  children: React.ReactNode;
}) {
  const pathname = usePathname();

  return (
    <div className="relative min-h-screen overflow-hidden text-slate-950">
      <div className="pointer-events-none absolute inset-x-0 top-0 h-[34rem] bg-[radial-gradient(circle_at_top,rgba(133,177,255,0.22),transparent_32rem)]" />
      <div className="pointer-events-none absolute right-[-8rem] top-28 h-72 w-72 rounded-full bg-[radial-gradient(circle,rgba(255,255,255,0.95),rgba(255,255,255,0))]" />
      <div className="pointer-events-none absolute left-[-4rem] top-36 h-56 w-56 rounded-full bg-[radial-gradient(circle,rgba(183,210,255,0.35),rgba(183,210,255,0))]" />

      <header className="sticky top-0 z-40 px-4 pt-4 sm:px-6">
        <div className="portal-frame mx-auto flex max-w-7xl flex-col gap-4 px-5 py-4 sm:px-6 lg:flex-row lg:items-center lg:justify-between">
          <div className="flex items-center gap-4">
            <div className="flex h-11 w-11 items-center justify-center rounded-[1.2rem] bg-slate-950 text-white shadow-[0_12px_30px_rgba(15,23,42,0.2)]">
              <Cloud className="h-5 w-5" />
            </div>
            <div>
              <p className="portal-kicker">
                Stratus
              </p>
              <h1 className="text-lg font-semibold tracking-[-0.03em] text-slate-950">
                Operator Portal
              </h1>
            </div>
          </div>
          <div className="hidden items-center gap-3 lg:flex">
            <div className="rounded-full border border-slate-200/80 bg-white/70 px-3 py-1.5 text-xs font-medium text-slate-600">
              Read-only local cockpit
            </div>
          </div>
          <nav className="flex items-center gap-2 overflow-x-auto pb-1 lg:pb-0">
            {navItems.map(({ href, label, icon: Icon }) => (
              <Link
                key={href}
                href={href}
                className={`inline-flex items-center gap-2 rounded-full px-4 py-2 text-sm font-medium transition ${
                  pathname === href
                    ? "bg-slate-950 text-white shadow-[0_14px_30px_rgba(15,23,42,0.22)]"
                    : "border border-slate-200/80 bg-white/76 text-slate-700 hover:border-slate-300 hover:bg-white hover:text-slate-950"
                }`}
              >
                <Icon className="h-4 w-4" />
                {label}
              </Link>
            ))}
          </nav>
        </div>
      </header>
      <main className="mx-auto flex max-w-7xl flex-col gap-8 px-4 pb-12 pt-10 sm:px-6">
        {children}
      </main>
    </div>
  );
}
