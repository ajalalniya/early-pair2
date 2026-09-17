/**
 * The REST client. Every response is validated with the schema from
 * `types/api.ts`; a backend error body becomes an `ApiError` carrying the
 * backend's code and message verbatim, so views can say exactly why something
 * is unavailable (a 501 from a not-yet-built endpoint included).
 */
import type { z } from 'zod'
import {
  AddEndpointResponseSchema,
  AddTokenWatchResponseSchema,
  AddTopicWatchResponseSchema,
  ApproveResponseSchema,
  ApiErrorBodySchema,
  ChainInfoSchema,
  FundsSchema,
  ImportPoolResponseSchema,
  HoldingSchema,
  ListSchema,
  NonceResponseSchema,
  PoolPageSchema,
  PoolSchema,
  PositionSchema,
  QuoteSchema,
  RecordTradeResponseSchema,
  RpcHealthSchema,
  TokenWatchSchema,
  TopicWatchListSchema,
  TradersSchema,
  TxBuildResponseSchema,
  VerifyResponseSchema,
  type ExitQuoteRequest,
  type Pool,
  type RpcAction,
  type SwapQuoteRequest,
  type ZapQuoteRequest,
} from '@/types/api'
import { API_BASE } from './env'

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }

  get notImplemented(): boolean {
    return this.status === 501
  }
  get unauthorized(): boolean {
    return this.status === 401
  }
}

type TokenGetter = () => string | null
let getToken: TokenGetter = () => null
let onUnauthorized: () => void = () => {}

export function configureAuth(getter: TokenGetter, unauthorized: () => void): void {
  getToken = getter
  onUnauthorized = unauthorized
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE'
  body?: unknown
  auth?: boolean
  signal?: AbortSignal
}

async function request(path: string, opts: RequestOptions = {}): Promise<unknown> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
  if (opts.auth) {
    const token = getToken()
    if (!token) throw new ApiError(401, 'unauthenticated', 'Connect a wallet and sign in to use this.')
    headers.Authorization = `Bearer ${token}`
  }

  let res: Response
  try {
    res = await fetch(API_BASE + path, {
      method: opts.method ?? 'GET',
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: opts.signal,
    })
  } catch (err) {
    if ((err as Error).name === 'AbortError') throw err
    throw new ApiError(0, 'network', 'The EarlyPair API could not be reached.')
  }

  if (res.status === 204) return null
  const text = await res.text()
  let json: unknown = null
  if (text) {
    try {
      json = JSON.parse(text)
    } catch {
      throw new ApiError(res.status, 'bad_response', 'The API returned a response that is not JSON.')
    }
  }

  if (!res.ok) {
    const parsed = ApiErrorBodySchema.safeParse(json)
    const err = parsed.success
      ? new ApiError(res.status, parsed.data.error.code, parsed.data.error.message)
      : new ApiError(res.status, 'http_' + res.status, res.statusText || 'Request failed')
    if (res.status === 401 && opts.auth) onUnauthorized()
    throw err
  }
  return json
}

function parse<T extends z.ZodType>(schema: T, data: unknown, what: string): z.infer<T> {
  const r = schema.safeParse(data)
  if (!r.success) {
    console.warn(`[api] ${what} did not match the expected shape`, r.error.issues)
    throw new ApiError(0, 'bad_shape', `The API returned an unexpected ${what} payload.`)
  }
  return r.data
}

/** Validate a list item by item, so one malformed row cannot blank a feed. */
function parseEach<T extends z.ZodType>(schema: T, items: unknown[], what: string): z.infer<T>[] {
  const out: z.infer<T>[] = []
  for (const item of items) {
    const r = schema.safeParse(item)
    if (r.success) out.push(r.data)
    else console.warn(`[api] dropped a malformed ${what}`, r.error.issues)
  }
  return out
}

const qs = (params: Record<string, string | number | undefined | null>): string => {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') u.set(k, String(v))
  }
  const s = u.toString()
  return s ? `?${s}` : ''
}

export interface PoolListParams {
  chain?: string
  risk?: string
  minLiq?: number
  maxAgeMin?: number
  sort?: 'age' | 'liq' | 'vol'
  limit?: number
  cursor?: string | null
}

export const api = {
  async chains() {
    const body = parse(ListSchema(ChainInfoSchema), await request('/chains'), 'chains')
    return body.data
  },

  async pools(params: PoolListParams = {}, signal?: AbortSignal): Promise<{ data: Pool[]; nextCursor: string | null }> {
    const body = parse(PoolPageSchema, await request('/pools' + qs({ ...params }), { signal }), 'pool page')
    return { data: parseEach(PoolSchema, body.data, 'pool'), nextCursor: body.nextCursor }
  },

  async pool(chainId: string, address: string) {
    return parse(PoolSchema, await request(`/pools/${enc(chainId)}/${enc(address)}`), 'pool')
  },

  /**
   * Pools on any chain whose address starts with `q` (0x and at least 4 hex
   * digits), exact match first. Includes pairs removed from the feed.
   */
  async searchPools(q: string, signal?: AbortSignal): Promise<Pool[]> {
    const body = parse(PoolPageSchema, await request('/pools/search' + qs({ q }), { signal }), 'search results')
    return parseEach(PoolSchema, body.data, 'search result')
  },

  /** Starred pairs, most recently starred first. Shared: no sign-in needed. */
  async favorites(signal?: AbortSignal): Promise<Pool[]> {
    const body = parse(PoolPageSchema, await request('/favorites', { signal }), 'favorites')
    return parseEach(PoolSchema, body.data, 'favorite')
  },

  async addFavorite(chainId: string, address: string) {
    return parse(PoolSchema, await request(`/favorites/${enc(chainId)}/${enc(address)}`, { method: 'PUT' }), 'favorite')
  },

  /** Add a pool by address: the backend reads it from chain, stores it and stars it. */
  async importPool(chainId: string, address: string) {
    const body = await request('/pools/import', { method: 'POST', body: { chainId, address } })
    return parse(ImportPoolResponseSchema, body, 'added pool')
  },

  async removeFavorite(chainId: string, address: string) {
    await request(`/favorites/${enc(chainId)}/${enc(address)}`, { method: 'DELETE' })
  },

  async traders(chainId: string, address: string, window = '1h') {
    return parse(TradersSchema, await request(`/pools/${enc(chainId)}/${enc(address)}/traders${qs({ window })}`), 'traders')
  },

  async rpcHealth() {
    return parse(RpcHealthSchema, await request('/rpc/health'), 'rpc health')
  },

  async addEndpoint(body: { chainId: string; transport: 'http' | 'ws'; url: string }) {
    return parse(AddEndpointResponseSchema, await request('/rpc/endpoints', { method: 'POST', body }), 'endpoint')
  },

  async endpointAction(id: string, action: RpcAction) {
    return (await request(`/rpc/endpoints/${enc(id)}/action`, { method: 'POST', body: { action } })) as {
      ok?: boolean
      head?: number
      note?: string
    }
  },

  async nonce(address: string) {
    return parse(NonceResponseSchema, await request('/auth/nonce', { method: 'POST', body: { address } }), 'nonce')
  },

  async verify(body: { address: string; nonce: string; signature: string }) {
    return parse(VerifyResponseSchema, await request('/auth/verify', { method: 'POST', body }), 'session')
  },

  async funds() {
    return parse(FundsSchema, await request('/portfolio/funds', { auth: true }), 'funds')
  },

  async holdings() {
    return parse(ListSchema(HoldingSchema), await request('/portfolio/holdings', { auth: true }), 'holdings').data
  },

  /** Remove a token from "Tokens bought". Its trades are kept; a later trade in it lists it again. */
  async hideHolding(chainId: string, token: string) {
    await request(`/portfolio/holdings/${enc(chainId)}/${enc(token)}`, { method: 'DELETE', auth: true })
  },

  /** Report a confirmed swap; the backend verifies it from the receipt before recording it. */
  async recordTrade(body: { chainId: string; pool: string; txHash: string }) {
    return parse(RecordTradeResponseSchema, await request('/portfolio/trades', { method: 'POST', body }), 'recorded trade')
  },

  async positions() {
    return parse(ListSchema(PositionSchema), await request('/portfolio/positions', { auth: true }), 'positions').data
  },

  async quoteSwap(body: SwapQuoteRequest, signal?: AbortSignal) {
    return parse(QuoteSchema, await request('/quote/swap', { method: 'POST', body, signal }), 'swap quote')
  },

  async quoteZap(body: ZapQuoteRequest, signal?: AbortSignal) {
    return parse(QuoteSchema, await request('/quote/zap', { method: 'POST', body, signal }), 'zap quote')
  },

  async quoteExit(body: ExitQuoteRequest, signal?: AbortSignal) {
    return parse(QuoteSchema, await request('/quote/exit', { method: 'POST', body, signal }), 'exit quote')
  },

  async buildTx(body: Record<string, unknown>) {
    return parse(TxBuildResponseSchema, await request('/tx/build', { method: 'POST', body }), 'transaction plan')
  },

  /** The approvals selling (or buying) in a pool will need, sized to the wallet's balance. */
  async approveAhead(body: { chainId: string; pool: string; from: string; side?: 'buy' | 'sell' }) {
    return parse(ApproveResponseSchema, await request('/tx/approve', { method: 'POST', body }), 'approval plan')
  },

  async tokenWatches() {
    return parse(ListSchema(TokenWatchSchema), await request('/watch/tokens', { auth: true }), 'token watches').data
  },

  async addTokenWatch(name: string) {
    return parse(AddTokenWatchResponseSchema, await request('/watch/tokens', { method: 'POST', body: { name }, auth: true }), 'token watch')
  },

  async deleteTokenWatch(id: number) {
    await request(`/watch/tokens/${id}`, { method: 'DELETE', auth: true })
  },

  async topicWatches() {
    return parse(TopicWatchListSchema, await request('/watch/topics', { auth: true }), 'topic watches')
  },

  async addTopicWatch(signature: string) {
    return parse(AddTopicWatchResponseSchema, await request('/watch/topics', { method: 'POST', body: { signature }, auth: true }), 'topic watch')
  },

  async deleteTopicWatch(id: number) {
    await request(`/watch/topics/${id}`, { method: 'DELETE', auth: true })
  },
}

function enc(s: string): string {
  return encodeURIComponent(s)
}

export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message
  if (err instanceof Error) return err.message
  return 'Something went wrong.'
}
