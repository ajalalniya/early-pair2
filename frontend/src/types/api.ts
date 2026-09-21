/**
 * Every payload the frontend exchanges with the EarlyPair backend.
 *
 * Schemas are the source of truth and types are inferred from them, so a
 * REST response and a WebSocket frame are validated against exactly the shape
 * the components are typed with. Nullable market fields are deliberate: the
 * backend says "not known yet" with null, and the UI renders an em dash.
 */
import { z } from 'zod'

const nullableNumber = z.number().nullish().transform((v) => v ?? null)
const nullableInt = z.number().int().nullish().transform((v) => v ?? null)

// ---------------------------------------------------------------- chains

export const ChainKindSchema = z.enum(['evm', 'solana', 'sui'])

export const ChainInfoSchema = z.object({
  id: z.string(),
  name: z.string(),
  tag: z.string(),
  kind: ChainKindSchema,
  numericId: z.number().int().optional(),
  explorer: z.string().optional(),
  nativeSymbol: z.string().optional(),
  head: z.number().int().nonnegative().default(0),
  ingesting: z.boolean().default(false),
  /** The chain's quote assets, lowercase: a pair is priced in a stable, or else the native currency. */
  stables: z.array(z.string()).nullish().transform((v) => v ?? []),
  wrappedNative: z.string().optional(),
  /** The DEXes this chain's ingester watches, in config order; drives the DEX filter. */
  dexes: z.array(z.string()).nullish().transform((v) => v ?? []),
})
export type ChainInfo = z.infer<typeof ChainInfoSchema>

// ---------------------------------------------------------------- pools

export const AmmModelSchema = z.enum(['v2', 'clmm', 'stable'])
export type AmmModel = z.infer<typeof AmmModelSchema>

export const PoolStateSchema = z.enum(['unconfirmed', 'confirmed', 'reorged'])
export type PoolState = z.infer<typeof PoolStateSchema>

export const TokenSchema = z.object({
  address: z.string(),
  symbol: z.string(),
  name: z.string().optional(),
  decimals: z.number().int().nonnegative(),
  totalSupply: z.string().optional(),
})
export type Token = z.infer<typeof TokenSchema>

export const RiskCheckSchema = z.object({
  label: z.string(),
  value: z.string(),
  ok: z.boolean(),
  known: z.boolean().default(true),
})
export type RiskCheck = z.infer<typeof RiskCheckSchema>

export const RiskSummarySchema = z.object({
  score: z.number().int().min(0).max(100),
  band: z.string(),
  verdict: z.string().default(''),
  checks: z.array(RiskCheckSchema).nullish().transform((v) => v ?? []),
  scoredAt: z.string().optional(),
  complete: z.boolean().default(true),
})
export type RiskSummary = z.infer<typeof RiskSummarySchema>

export const PoolSchema = z.object({
  chainId: z.string(),
  address: z.string(),
  pair: z.string(),
  dex: z.string(),
  model: AmmModelSchema,
  state: PoolStateSchema,
  createdAt: z.string(),
  createdBlock: z.number().int().nonnegative(),
  createdTx: z.string(),
  backfilled: z.boolean().optional(),
  token0: TokenSchema.nullish().transform((v) => v ?? null),
  token1: TokenSchema.nullish().transform((v) => v ?? null),
  feeTierBps: nullableInt,
  liquidityUsd: nullableNumber,
  volume1hUsd: nullableNumber,
  /**
   * The last minute's volume over this pair's average minute so far: 3.2 means
   * it is trading at 3.2x its usual pace. Null until there is enough history.
   */
  volAccel: nullableNumber,
  priceChange1h: nullableNumber,
  buyers1h: nullableInt,
  sellers1h: nullableInt,
  /**
   * Swaps whose trader the backend has not looked up yet. While above zero,
   * buyers1h and sellers1h are a floor ("27+"), not a final count.
   */
  tradersPending: nullableInt,
  /** Set on a starred pair, which the backend watches until it is unstarred. */
  favoritedAt: z.string().nullish().transform((v) => v ?? null),
  /** Set on a pair the backend's liquidity/volume screen removed from the feed. Only search and direct lookups return one. */
  removedAt: z.string().nullish().transform((v) => v ?? null),
  /** Set when a user added this pool by address; its figures start from then, not from the pair's launch. */
  importedAt: z.string().nullish().transform((v) => v ?? null),
  /**
   * Spot price of token0 in token1. Not emitted by the current backend; the
   * concentrated-liquidity range tool stays disabled until it is.
   */
  price: nullableNumber,
  risk: RiskSummarySchema.nullish().transform((v) => v ?? null),
})
export type Pool = z.infer<typeof PoolSchema>

/** What POST /pools/import answers: the stored pool, and whether this call created it. */
export const ImportPoolResponseSchema = z.object({
  pool: PoolSchema,
  added: z.boolean(),
})

export const PoolPageSchema = z.object({
  data: z.array(z.unknown()),
  nextCursor: z.string().nullish().transform((v) => v || null),
})

/**
 * A partial pools.update, which carries no market data: a confirmation or reorg
 * (`state`), a pair removed by the feed's liquidity and volume screen
 * (`removed`), or a pair starred or unstarred (`favoritedAt`, null when
 * unstarred).
 */
export const PoolStateUpdateSchema = z
  .object({
    chainId: z.string(),
    address: z.string(),
    state: PoolStateSchema.optional(),
    removed: z.boolean().optional(),
    favoritedAt: z.string().nullable().optional(),
  })
  .refine((u) => u.state !== undefined || u.removed !== undefined || u.favoritedAt !== undefined, {
    message: 'a pools.update carries no change',
  })
export type PoolStateUpdate = z.infer<typeof PoolStateUpdateSchema>

/** One step of a concentrated-liquidity depth chart. */
export const LiquidityBinSchema = z.object({
  tickLower: z.number().int(),
  tickUpper: z.number().int(),
  priceLower: z.number(),
  priceUpper: z.number(),
  /** L, the pool's own unit: what makes bins comparable with each other. */
  liquidity: z.number(),
  amount0: z.number(),
  amount1: z.number(),
  /** The bin holding the current price: the liquidity a trade meets first. */
  active: z.boolean(),
})
export type LiquidityBin = z.infer<typeof LiquidityBinSchema>

export const LiquidityDistributionSchema = z.object({
  tick: z.number().int(),
  price: z.number(),
  tickSpacing: z.number().int(),
  liquidity: z.number(),
  bins: z.array(LiquidityBinSchema).nullish().transform((v) => v ?? []),
  /** The scan reached its budget: the chart's edges are its own, not the pool's. */
  truncated: z.boolean().default(false),
  /**
   * The dollar price of the token this pool is priced in, and the pool's own
   * fee tier. Neither is needed to draw the depth chart; both are needed to
   * say what a dollar deposited into a range would earn.
   */
  quoteUsd: nullableNumber,
  feeBps: nullableInt,
})
export type LiquidityDistribution = z.infer<typeof LiquidityDistributionSchema>

export const TradersSchema = z.object({
  window: z.string(),
  buyers: z.number().int(),
  sellers: z.number().int(),
  volumeUsd: nullableNumber,
  ratio: nullableNumber,
  ratioInfinite: z.boolean().optional(),
})
export type Traders = z.infer<typeof TradersSchema>

// ---------------------------------------------------------------- rpc

export const RpcStateSchema = z.enum(['HEALTHY', 'DEGRADED', 'UNREACHABLE'])
export type RpcState = z.infer<typeof RpcStateSchema>

export const EndpointStatusSchema = z.object({
  id: z.string(),
  chainId: z.string(),
  transport: z.enum(['http', 'ws']),
  url: z.string(),
  state: RpcStateSchema,
  latencyMs: nullableInt,
  p50Ms: nullableInt,
  p95Ms: nullableInt,
  block: nullableInt,
  lagBlocks: z.number().int().default(0),
  subscriptions: z.number().int().default(0),
  uptimeSec: nullableInt,
  reconnects24h: z.number().int().default(0),
  pollIntervalMs: z.number().int().optional(),
  demotedUntil: z.string().optional(),
  lastError: z.string().optional(),
  successRate: z.number().optional(),
})
export type EndpointStatus = z.infer<typeof EndpointStatusSchema>

export const RpcSummarySchema = z.object({
  healthy: z.number().int(),
  total: z.number().int(),
  medianP50Ms: nullableInt,
  socketsLive: z.number().int(),
  socketsTotal: z.number().int(),
  subscriptions: z.number().int(),
  failoverPolicy: z.string(),
})
export type RpcSummary = z.infer<typeof RpcSummarySchema>

export const RpcHealthSchema = z.object({
  endpoints: z.array(EndpointStatusSchema),
  summary: RpcSummarySchema,
})
export type RpcHealth = z.infer<typeof RpcHealthSchema>

export const RpcActionSchema = z.enum(['resubscribe', 'ping', 'failover'])
export type RpcAction = z.infer<typeof RpcActionSchema>

export const AddEndpointResponseSchema = z.object({
  id: z.string(),
  chainId: z.string(),
  transport: z.enum(['http', 'ws']),
  url: z.string(),
  note: z.string().optional(),
})

// ---------------------------------------------------------------- auth

export const NonceResponseSchema = z.object({
  nonce: z.string(),
  expiresAt: z.string(),
  message: z.string(),
})
export const VerifyResponseSchema = z.object({
  token: z.string(),
  expiresAt: z.string(),
  address: z.string(),
})
export type Session = z.infer<typeof VerifyResponseSchema>

// ---------------------------------------------------------------- portfolio

export const FundsAssetSchema = z.object({
  symbol: z.string(),
  address: z.string().optional(),
  amount: z.string(),
  decimals: z.number().int(),
  usd: nullableNumber,
  isStable: z.boolean(),
  isNative: z.boolean(),
  /** The balance could not be read from the chain: show a dash, not a zero. */
  unavailable: z.boolean().default(false),
})
export type FundsAsset = z.infer<typeof FundsAssetSchema>

export const FundsChainSchema = z.object({
  chainId: z.string(),
  name: z.string(),
  tag: z.string(),
  assets: z.array(FundsAssetSchema),
  totalUsd: nullableNumber,
})
export type FundsChain = z.infer<typeof FundsChainSchema>

export const FundsSchema = z.object({
  chains: z.array(FundsChainSchema),
  stables: nullableNumber,
  totalUsd: nullableNumber,
})
export type Funds = z.infer<typeof FundsSchema>

/**
 * Holdings and LP positions are not implemented by the backend yet (it answers
 * 501). These shapes are the contract the views are built against.
 */
export const HoldingSchema = z.object({
  chainId: z.string(),
  pool: z.string().nullish().transform((v) => v ?? null),
  token: TokenSchema,
  amount: z.string(),
  avgEntryUsd: nullableNumber,
  priceUsd: nullableNumber,
  valueUsd: nullableNumber,
  pnlPct: nullableNumber,
  pnlUsd: nullableNumber,
  risk: RiskSummarySchema.nullish().transform((v) => v ?? null),
  /**
   * Whether selling the whole balance needs no approval first. False means the
   * sell would need approvals the wallet can sign ahead; null means unknown.
   */
  sellReady: z.boolean().nullish().transform((v) => v ?? null),
  /** Why a figure is missing, when one is (e.g. tokens held that were not bought here). */
  note: z.string().optional(),
})
export type Holding = z.infer<typeof HoldingSchema>

export const RecordTradeResponseSchema = z.object({
  recorded: z.boolean(),
  side: z.enum(['buy', 'sell']),
  token: z.string(),
  tokenAmount: z.string(),
})

export const PositionSchema = z.object({
  id: z.string(),
  chainId: z.string(),
  pool: z.string(),
  pair: z.string(),
  dex: z.string(),
  model: AmmModelSchema,
  inRange: z.boolean().nullish().transform((v) => v ?? null),
  valueUsd: nullableNumber,
  feesUsd: nullableNumber,
  sharePct: nullableNumber,
  aprPct: nullableNumber,
  quoteSymbol: z.string(),
  risk: RiskSummarySchema.nullish().transform((v) => v ?? null),
})
export type Position = z.infer<typeof PositionSchema>

export const ListSchema = <T extends z.ZodType>(item: T) => z.object({ data: z.array(item) })

// ---------------------------------------------------------------- quotes & tx

export const QuoteLegSchema = z.object({
  step: z.string(),
  label: z.string(),
  value: z.string(),
})
export type QuoteLeg = z.infer<typeof QuoteLegSchema>

export const QuoteSchema = z.object({
  legs: z.array(QuoteLegSchema),
  amountIn: z.string(),
  amountOut: z.string(),
  minReceived: z.string(),
  priceImpactBps: z.number().int(),
  rate: z.string().default(''),
  estGas: z.number().int().default(0),
  gasPriceWei: z.string().default(''),
  estGasNative: z.string().default(''),
  estGasUsd: nullableNumber,
  expiresAt: z.string(),
  gated: z.boolean().default(false),
  gateReason: z.string().optional(),
  // Zap and exit extras; absent on swap quotes.
  feeTierBps: nullableInt,
  estAprPct: nullableNumber,
  poolSharePctAfter: nullableNumber,
  lpSlipBps: nullableInt,
  feesClaimedUsd: nullableNumber,
  amountOutUsd: nullableNumber,
  remainingUsd: nullableNumber,
  quoteSymbol: z.string().optional(),
  quoteDecimals: z.number().int().optional(),
})
export type Quote = z.infer<typeof QuoteSchema>

export const UnsignedTxSchema = z.object({
  chainId: z.string(),
  to: z.string(),
  data: z.string(),
  value: z.string(),
  gas: z.number().int().optional(),
  kind: z.string(),
  label: z.string(),
})
export type UnsignedTx = z.infer<typeof UnsignedTxSchema>

export const TxBuildResponseSchema = z.object({
  steps: z.array(UnsignedTxSchema),
  expiresAt: z.string(),
  note: z.string().optional(),
})
export type TxBuildResponse = z.infer<typeof TxBuildResponseSchema>

/** POST /tx/approve: only the approvals a trade will need, signed ahead of it. */
export const ApproveResponseSchema = z.object({
  steps: z.array(UnsignedTxSchema),
  approved: z.boolean(),
  expiresAt: z.string().optional(),
  note: z.string().optional(),
})
export type ApproveResponse = z.infer<typeof ApproveResponseSchema>

export interface SwapQuoteRequest {
  chainId: string
  pool: string
  side: 'buy' | 'sell'
  amountIn: string
  slippageBps: number
  riskThreshold?: number
  /** Buy with ETH on a pair priced in WETH; the router wraps it in the swap. */
  payNative?: boolean
}

export interface ZapQuoteRequest {
  chainId: string
  pool: string
  amountIn: string
  splitBps: number
  slippageBps: number
}

export interface ExitQuoteRequest {
  chainId: string
  positionId: string
  pct: number
  slippageBps: number
}

// ---------------------------------------------------------------- watchlist

export const TokenWatchHitSchema = z.object({
  chainId: z.string(),
  pool: z.string(),
  pair: z.string().default(''),
  dex: z.string().default(''),
  seenAt: z.string(),
})
export type TokenWatchHit = z.infer<typeof TokenWatchHitSchema>

export const TokenWatchSchema = z.object({
  id: z.number().int(),
  name: z.string(),
  address: z.string().default(''),
  chains: z.array(z.string()).nullish().transform((v) => v ?? []),
  resolved: z.boolean(),
  createdAt: z.string(),
  resolvedHits: z.array(TokenWatchHitSchema).nullish().transform((v) => v ?? []),
})
export type TokenWatch = z.infer<typeof TokenWatchSchema>

export const AddTokenWatchResponseSchema = z.object({
  watch: TokenWatchSchema.omit({ resolvedHits: true }).extend({
    resolvedHits: z.array(TokenWatchHitSchema).optional(),
  }),
  status: z.string().optional(),
  message: z.string().optional(),
})

export const TopicWatchSchema = z.object({
  id: z.number().int(),
  name: z.string(),
  signature: z.string().default(''),
  topic0: z.string(),
  chains: z.array(z.string()).nullish().transform((v) => v ?? []),
  alerts: z.boolean().default(false),
  suspended: z.boolean().default(false),
  createdAt: z.string(),
  matches24h: z.number().int().default(0),
  lastMatchAt: z.string().nullish().transform((v) => v ?? null),
  lastTx: z.string().default(''),
  /** Filled client-side from stream hits so the last tx can link to its explorer. */
  lastChainId: z.string().optional(),
})
export type TopicWatch = z.infer<typeof TopicWatchSchema>

export const TopicWatchListSchema = z.object({
  data: z.array(TopicWatchSchema),
  limit: z.number().int().optional(),
})

export const AddTopicWatchResponseSchema = z.object({
  watch: TopicWatchSchema,
  note: z.string().optional(),
})

// ---------------------------------------------------------------- stream

export const StreamTopicSchema = z.enum([
  'pools.new',
  'pools.update',
  'rpc.health',
  'watch.hits',
  'head',
  'portfolio',
  'resync',
])
export type StreamTopic = z.infer<typeof StreamTopicSchema>

/** Envelope: `{ t, seq, ts, d, chain }`. `d` is validated per topic. */
export const FrameSchema = z.object({
  t: z.string(),
  seq: z.number().int().nonnegative().optional(),
  ts: z.number().optional(),
  d: z.unknown().optional(),
  chain: z.string().optional(),
})
export type Frame = z.infer<typeof FrameSchema>

export const SubscribedAckSchema = z.object({
  t: z.literal('subscribed'),
  topics: z.array(z.string()).nullish(),
  chains: z.array(z.string()).nullish(),
  seq: z.number().int().nonnegative(),
  resync: z.boolean().optional(),
  reason: z.string().optional(),
  replayed: z.number().int().optional(),
})

export const HeadDataSchema = z.object({
  chainId: z.string(),
  block: z.number().int().nonnegative(),
  timestamp: z.string().optional(),
})
export type HeadData = z.infer<typeof HeadDataSchema>

export const WatchHitDataSchema = z.discriminatedUnion('kind', [
  z.object({
    kind: z.literal('token'),
    watchId: z.number().int(),
    name: z.string(),
    chainId: z.string(),
    pool: z.string(),
    pair: z.string(),
    dex: z.string(),
    at: z.string(),
  }),
  z.object({
    kind: z.literal('topic'),
    topic0: z.string(),
    chainId: z.string(),
    txHash: z.string(),
    blockNumber: z.number().int(),
    at: z.string(),
  }),
])
export type WatchHitData = z.infer<typeof WatchHitDataSchema>

export const ApiErrorBodySchema = z.object({
  error: z.object({ code: z.string(), message: z.string() }),
})
