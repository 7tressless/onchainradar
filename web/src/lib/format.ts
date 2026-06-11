export function shortAddr(a: string, head = 6, tail = 4): string {
  if (!a) return "";
  return a.length <= head + tail + 1 ? a : `${a.slice(0, head)}…${a.slice(-tail)}`;
}

// Format a raw big-integer token amount (decimal string) using its decimals.
// Per the API contract amounts are raw integer strings; anything else is shown
// as-is (never silently mangled), and grouping avoids Number() precision loss.
export function fmtAmount(raw: string, decimals: number, maxFrac = 2): string {
  if (!raw) return "0";
  if (!/^-?\d+$/.test(raw)) return raw;
  const neg = raw.startsWith("-");
  const s = neg ? raw.slice(1) : raw;
  const padded = s.padStart(decimals + 1, "0");
  const intPart = padded.slice(0, padded.length - decimals) || "0";
  const fracRaw = decimals > 0 ? padded.slice(padded.length - decimals) : "";
  const frac = fracRaw.slice(0, maxFrac).replace(/0+$/, "");
  const grouped = intPart
    .replace(/^0+(?=\d)/, "")
    .replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return (neg ? "-" : "") + grouped + (frac ? `.${frac}` : "");
}

// LLM notes trail with a confidence word; split it out for a clean display + tag.
export function parseNote(note: string | null): { text: string; confidence: string | null } {
  if (!note) return { text: "", confidence: null };
  const m = note.match(/(?:confidence:\s*)?(high|medium|low)\.?\s*$/i);
  if (m && m.index !== undefined) {
    return {
      text: note.slice(0, m.index).replace(/[·\-,\s]+$/, "").trim(),
      confidence: m[1].toUpperCase(),
    };
  }
  return { text: note.trim(), confidence: null };
}

// Compact USD from a decimal string ("45200000" → "$45.2M").
export function fmtUsd(s: string | number): string {
  const n = typeof s === "string" ? Number(s) : s;
  if (!Number.isFinite(n)) return "—";
  if (n >= 1e9) return `$${(n / 1e9).toFixed(1)}B`;
  if (n >= 1e6) return `$${(n / 1e6).toFixed(1)}M`;
  if (n >= 1e3) return `$${(n / 1e3).toFixed(1)}K`;
  return `$${n.toFixed(0)}`;
}

// Compact relative time. Client-only (uses Date.now); render via <RelTime/> so it
// stays out of the server render (avoids hydration drift).
export function relTime(iso: string, now: number = Date.now()): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "";
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}
