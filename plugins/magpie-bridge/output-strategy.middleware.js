// Inject selected output styles into Chat Completions, Anthropic and Responses
// requests. Existing markers, empty selections and unknown request shapes pass
// through. No telemetry, filesystem access or asynchronous hook is involved.
// 上游原样拷贝自 magpie-community/plugins packages/output-strategy（MIT，
// v0.2.1，2026-11-10 经 magpie-bridge 适配层接入 yardmaster）。
import { applyOutputStyles } from "./apply.js"
export function onRequest(body, ctx) {
  try {
    const result = applyOutputStyles(body, { ...ctx?.options, protocol: ctx?.protocol })
    return result.applied ? result.body : undefined
  } catch {
    return undefined
  }
}
