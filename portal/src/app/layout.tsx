import type { Metadata } from "next";
import { IBM_Plex_Mono, Instrument_Serif, Manrope } from "next/font/google";
import { PortalShell } from "@/components/portal-shell";
import "./globals.css";

const sans = Manrope({
  variable: "--font-sans-main",
  subsets: ["latin"],
});

const mono = IBM_Plex_Mono({
  variable: "--font-mono-main",
  subsets: ["latin"],
  weight: ["400", "500"],
});

const serif = Instrument_Serif({
  variable: "--font-serif-display",
  subsets: ["latin"],
  weight: "400",
});

export const metadata: Metadata = {
  title: "Stratus Operator Portal",
  description: "Read-only local operator cockpit for Stratus",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="en"
      className={`${sans.variable} ${mono.variable} ${serif.variable} h-full antialiased`}
    >
      <body className="min-h-full flex flex-col">
        <PortalShell>{children}</PortalShell>
      </body>
    </html>
  );
}
