import { keccak256, toBytes } from 'viem'

export type TopicInput =
  | { kind: 'topic'; topic0: string; name: string; signature: '' }
  | { kind: 'signature'; topic0: string; name: string; signature: string }
  | { kind: 'invalid'; reason: string }

/** Strip whitespace and parameter names the way the backend does. */
export function normalizeSignature(sig: string): string {
  const s = sig.trim().replace(/^event\s+/, '')
  const open = s.indexOf('(')
  if (open <= 0 || !s.endsWith(')')) return s.replace(/\s+/g, '')
  const name = s.slice(0, open).trim()
  const params = s
    .slice(open + 1, -1)
    .split(',')
    .map((p) => p.trim())
    .filter(Boolean)
    .map((p) => p.replace(/\s+indexed\b/, '').split(/\s+/)[0] ?? '')
  return `${name}(${params.join(',')})`
}

/**
 * Accepts a raw 32-byte topic0 or a human event signature and hashes the
 * latter locally, so the UI can show topic0 instantly while the backend
 * confirms.
 */
export function parseTopicInput(input: string): TopicInput {
  const v = input.trim()
  if (!v) return { kind: 'invalid', reason: 'Enter an event signature or a topic hash.' }
  if (/^0x[0-9a-fA-F]{64}$/.test(v)) {
    return { kind: 'topic', topic0: v.toLowerCase(), name: v.slice(0, 10) + '…', signature: '' }
  }
  if (v.startsWith('0x')) return { kind: 'invalid', reason: 'A topic hash is 0x followed by 64 hex characters.' }
  const normalized = normalizeSignature(v)
  if (!/^[A-Za-z_$][\w$]*\([\w,[\]()]*\)$/.test(normalized)) {
    return { kind: 'invalid', reason: 'Signature must look like Name(type,type).' }
  }
  return {
    kind: 'signature',
    topic0: keccak256(toBytes(normalized)),
    name: normalized.slice(0, normalized.indexOf('(')),
    signature: normalized,
  }
}
