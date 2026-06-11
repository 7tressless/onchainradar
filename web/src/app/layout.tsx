import type { Metadata } from "next";
import { Archivo, Space_Mono } from "next/font/google";
import { Providers } from "./providers";
import "./globals.css";

// Archivo = huge brutalist display; Space Mono = technical / data text.
const display = Archivo({
  subsets: ["latin"],
  weight: ["400", "600", "700", "800"], // only the weights actually used, for a smaller payload
  variable: "--font-archivo",
  display: "swap",
});

const mono = Space_Mono({
  subsets: ["latin"],
  weight: ["400", "700"],
  variable: "--font-space-mono",
  display: "swap",
});

export const metadata: Metadata = {
  title: "OCR · OnChain Radar",
  description:
    "Autonomous AI analyst for Mantle. Detects abnormal DeFi flows in real time and attests every signal on-chain: a verifiable, tamper-proof track record.",
  robots: { index: false, follow: false },
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className={`${display.variable} ${mono.variable}`}>
      <body>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
