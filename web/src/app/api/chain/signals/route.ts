import { NextResponse } from "next/server";
import { fetchRecentSignals } from "@/lib/chain";

export const dynamic = "force-dynamic";

// Same-origin route for the client (avoids browser↔RPC CORS). Returns attestations
// read straight from the chain.
export async function GET() {
  try {
    const signals = await fetchRecentSignals(60);
    return NextResponse.json({ signals });
  } catch (e) {
    // Log the cause and return a generic message. Upstream error text can carry
    // internal details (e.g. a keyed RPC URL), so it must not reach the client.
    console.error("chain signals read failed:", e);
    return NextResponse.json({ error: "chain read failed" }, { status: 502 });
  }
}
