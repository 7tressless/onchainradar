import { NextResponse } from "next/server";
import { getHeadBlock } from "@/lib/chain";

export const dynamic = "force-dynamic";

export async function GET() {
  try {
    return NextResponse.json({ block: await getHeadBlock() });
  } catch (e) {
    // Log the cause and return a generic message. Upstream error text can carry
    // internal details (e.g. a keyed RPC URL), so it must not reach the client.
    console.error("chain block read failed:", e);
    return NextResponse.json({ error: "chain read failed" }, { status: 502 });
  }
}
